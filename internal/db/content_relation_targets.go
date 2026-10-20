package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The foreign key on a relation column is not tenant scoped: every tenant's
// rows share the target table, so the key accepts a row another tenant owns.
// Every write that sets a relation therefore checks its targets against the
// writing tenant first, in the transaction that writes them, through the same
// check the plugins that write content with their own SQL use.

// insertRelationValues returns the relation values an insert writes: the
// ones data carries, and the declared default of a relation data leaves out,
// which buildInsert writes as well.
func insertRelationValues(sc *domain.Schema, data map[string]any) []core.RelationValue {
	values := core.RelationValues(sc.Fields, data)
	for _, f := range sc.Fields {
		if f.System || f.FieldType != "relation" || isVirtualRelationField(f) || f.RelationTo == "" || f.Default == nil {
			continue
		}
		if _, ok := data[fieldColumn(f)]; ok {
			continue
		}
		if _, ok := data[f.Name]; ok {
			continue
		}
		values = append(values, core.RelationValue{Field: f.Name, Target: f.RelationTo, Value: f.Default})
	}
	return values
}

// setsRelation reports whether any value names a row. A null clears its
// relation and needs no check.
func setsRelation(values []core.RelationValue) bool {
	for _, v := range values {
		if v.Value != nil {
			return true
		}
	}
	return false
}

// writeChecked runs write after confirming every relation value in values
// names a row of the caller's tenant. When a value names one, the check and
// the write share a transaction and the targets stay locked until it ends, so
// a target deleted or moved to another tenant in between fails the write
// rather than slipping past the check.
//
// A refusal wraps domain.ErrValidation and a *core.RelationTargetError naming
// the field. It is the same for a row another tenant holds and a row nobody
// holds.
func (s *ContentStore) writeChecked(ctx context.Context, values []core.RelationValue, write func(ctx context.Context) error) error {
	if !setsRelation(values) {
		return write(ctx)
	}
	return s.inTransactionChecked(ctx, values, write)
}

// inTransactionChecked is writeChecked for a write that needs a transaction
// of its own whatever it carries.
func (s *ContentStore) inTransactionChecked(ctx context.Context, values []core.RelationValue, write func(ctx context.Context) error) error {
	if err := s.ensureTargetTables(ctx, values); err != nil {
		return err
	}
	return WriteInTransaction(ctx, s.pool, func(ctx context.Context) error {
		if setsRelation(values) {
			if err := s.checkRelationTargets(ctx, values); err != nil {
				return err
			}
		}
		return write(ctx)
	})
}

// ensureTargetTables gives every table a check reads its tenant column. It
// runs before the transaction opens: on MySQL a column added inside one
// would commit whatever the transaction had written so far.
func (s *ContentStore) ensureTargetTables(ctx context.Context, values []core.RelationValue) error {
	for _, v := range values {
		if v.Value == nil {
			continue
		}
		if err := s.ensureTable(ctx, domain.TableName(v.Target)); err != nil {
			return fmt.Errorf("check relation %s: %w", v.Field, err)
		}
	}
	return nil
}

// checkRelationTargets runs the check on the store's pool, which carries the
// transaction a caller opened on ctx.
func (s *ContentStore) checkRelationTargets(ctx context.Context, values []core.RelationValue) error {
	err := core.CheckRelationTargets(ctx, poolQuerier{pool: s.pool}, s.pool.Engine(), tenant.ID(ctx), values)
	var refused *core.RelationTargetError
	if errors.As(err, &refused) {
		return fmt.Errorf("%w: %w", domain.ErrValidation, refused)
	}
	return err
}

// poolQuerier hands the pool to a pkg/core helper that reads through
// core.Querier. Every statement goes through the pool, so a transaction a
// caller opened on ctx, the placeholder rewrite and the table qualification
// all apply.
type poolQuerier struct{ pool DB }

func (q poolQuerier) QueryRow(ctx context.Context, query string, args ...any) (core.Row, error) {
	row, err := q.pool.QueryRow(ctx, query, args...)
	if err != nil {
		return core.ErrorRow(err), err
	}
	return row, nil
}

func (q poolQuerier) Query(ctx context.Context, query string, args ...any) (core.Rows, error) {
	rows, err := q.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return sqlRows{rows}, nil
}

func (q poolQuerier) Exec(ctx context.Context, query string, args ...any) (core.CommandTag, error) {
	res, err := q.pool.Exec(ctx, query, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected() // err suppressed: a driver that cannot count reports zero
	return core.CommandTag{RowsAffected: n}, nil
}

// Begin is refused. The helpers this adapter serves run inside the
// transaction the store opened, and a second one would be a different
// connection that sees none of it.
func (q poolQuerier) Begin(context.Context) (core.Tx, error) {
	return nil, errors.New("content store querier: begin a transaction on the store instead")
}

// sqlRows is a *sql.Rows as core.Rows, whose Close reports nothing.
type sqlRows struct{ rows *sql.Rows }

func (r sqlRows) Next() bool             { return r.rows.Next() }
func (r sqlRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r sqlRows) Close()                 { _ = r.rows.Close() } // err suppressed: Err carries what the read saw
func (r sqlRows) Err() error             { return r.rows.Err() }
