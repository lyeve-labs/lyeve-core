package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// A belongs_to relation keeps the id of the row it points at, and the foreign
// key the schema engine declares is the only thing the database checks about
// that id: some row of the target table holds it. Every tenant's rows share
// that table, so the key accepts a row another tenant owns, and its refusal of
// an id nobody holds tells a caller which ids exist in other tenants. A writer
// therefore checks each target here, inside the transaction that writes it.

// RelationValue is one relation value a write carries.
type RelationValue struct {
	// Field is the key the value was sent under. A refusal names it.
	Field string
	// Target is the schema the relation points at.
	Target string
	// Value is the value as the write carries it. Nil clears the relation
	// and is not checked.
	Value any
}

// RelationTargetError refuses a relation value that names no row the
// writing tenant holds in the target schema. A row another tenant holds, a
// row nobody holds and a value that is not an id are refused alike, so the
// refusal says nothing about other tenants. It is a validation failure:
// errors.Is(err, ErrValidation) holds.
type RelationTargetError struct {
	Field string
}

func (e *RelationTargetError) Error() string {
	return e.Field + ": the related entry does not exist"
}

// Is reports the refusal as ErrValidation.
func (e *RelationTargetError) Is(target error) bool { return target == ErrValidation }

// RelationValues returns the belongs_to values data carries for fields.
//
// A writer may take a belongs_to under the field's name or under its key
// column, so the value under each key data carries is returned, and data
// carrying both has both checked. A field data leaves out returns nothing,
// and so does every other kind of field: has_one, has_many and many_to_many
// keep no value on the row.
func RelationValues(fields []SchemaField, data map[string]any) []RelationValue {
	var out []RelationValue
	for _, f := range fields {
		if f.System || f.FieldType != "relation" || f.RelationTo == "" {
			continue
		}
		if f.RelationType != RelBelongsTo && f.RelationType != "" {
			continue
		}
		col := FKColumn(f.Name, f.RelationFKName)
		for _, key := range []string{f.Name, col} {
			if v, ok := data[key]; ok {
				out = append(out, RelationValue{Field: key, Target: f.RelationTo, Value: v})
			}
			if col == f.Name {
				break
			}
		}
	}
	return out
}

// relationTargetBatch bounds the ids one statement names. SQL Server takes at
// most 2100 parameters.
const relationTargetBatch = 1000

// CheckRelationTargets refuses the first value that names no row tenantID
// holds in its target schema's table, with a *RelationTargetError.
//
// Pass the tenant the write stamps on its row and the querier the write runs
// on. Every row the check finds stays locked against a delete or an update
// until that querier's transaction ends, so run it inside the transaction
// that writes the values: a target cannot then be deleted or move to another
// tenant between the check and the write. Outside a transaction the lock
// ends with the statement.
//
// The tenant is compared in Go as well as in the statement. MySQL and SQL
// Server compare the column without regard to case, and a row stored under
// another spelling of the tenant is not the tenant's row.
//
// A failure to read is returned as it is. A target table that does not
// exist holds no row, and the value is refused.
func CheckRelationTargets(ctx context.Context, q Querier, dialect, tenantID string, values []RelationValue) error {
	type target struct {
		ids    []uuid.UUID
		fields map[uuid.UUID]string
	}
	byTarget := map[string]*target{}
	var order []string
	for _, v := range values {
		id, present, ok := relationID(v.Value)
		if !present {
			continue
		}
		if !ok {
			return &RelationTargetError{Field: v.Field}
		}
		t := byTarget[v.Target]
		if t == nil {
			t = &target{fields: map[uuid.UUID]string{}}
			byTarget[v.Target] = t
			order = append(order, v.Target)
		}
		if _, seen := t.fields[id]; !seen {
			t.fields[id] = v.Field
			t.ids = append(t.ids, id)
		}
	}
	sort.Strings(order)
	for _, name := range order {
		t := byTarget[name]
		table := TableName(name)
		// A generated table lives in the engine's database. Registered, a
		// reference to it resolves there from a tenant-scoped connection too.
		sqlx.RegisterSharedTable(table)
		for start := 0; start < len(t.ids); start += relationTargetBatch {
			end := min(start+relationTargetBatch, len(t.ids))
			found, err := readRelationTargets(ctx, q, dialect, table, tenantID, t.ids[start:end])
			if err != nil {
				if sqlx.IsUndefinedTable(err) {
					return &RelationTargetError{Field: t.fields[t.ids[start]]}
				}
				return fmt.Errorf("check relation targets in %s: %w", table, err)
			}
			for _, id := range t.ids[start:end] {
				if !found[id] {
					return &RelationTargetError{Field: t.fields[id]}
				}
			}
		}
	}
	return nil
}

// readRelationTargets returns which of ids tenantID holds in table, locking
// the rows it finds.
func readRelationTargets(ctx context.Context, q Querier, dialect, table, tenantID string, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	args := make([]any, 0, len(ids)+1)
	args = append(args, tenantID)
	marks := make([]string, len(ids))
	for i, id := range ids {
		args = append(args, id.String())
		marks[i] = fmt.Sprintf("$%d", i+2)
	}
	rows, err := q.Query(ctx, relationTargetsQuery(dialect, QuoteIdentifier(dialect, table), strings.Join(marks, ", ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := make(map[uuid.UUID]bool, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var owner string
		if err := rows.Scan(ScanUUID(dialect, &id), &owner); err != nil {
			return nil, err
		}
		if owner == tenantID {
			found[id] = true
		}
	}
	return found, rows.Err()
}

// relationTargetsQuery reads the id and tenant of the listed rows of one
// tenant, taking a shared lock on each that lasts until the transaction ends.
// SQL Server spells the lock as a table hint, and the other two as a locking
// clause.
func relationTargetsQuery(dialect, table, marks string) string {
	const where = " WHERE tenant_id = $1 AND id IN ("
	if dialect == "mssql" {
		return "SELECT id, tenant_id FROM " + table + " WITH (REPEATABLEREAD, ROWLOCK)" + where + marks + ")"
	}
	return "SELECT id, tenant_id FROM " + table + where + marks + ") FOR SHARE"
}

// relationID reads a relation value as an id. present is false for a value
// that clears the relation, and ok is false for one that cannot be an id.
func relationID(v any) (id uuid.UUID, present, ok bool) {
	switch x := v.(type) {
	case nil:
		return uuid.Nil, false, false
	case uuid.UUID:
		return x, true, true
	case *uuid.UUID:
		if x == nil {
			return uuid.Nil, false, false
		}
		return *x, true, true
	case uuid.NullUUID:
		return x.UUID, x.Valid, x.Valid
	case string:
		parsed, err := uuid.Parse(x)
		return parsed, true, err == nil
	case []byte:
		parsed, err := uuid.ParseBytes(x)
		return parsed, true, err == nil
	}
	return uuid.Nil, true, false
}
