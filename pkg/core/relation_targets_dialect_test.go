//go:build !mutest

package core_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// relationTargetTable creates a table shaped like a generated content table
// for schema: an id key, the tenant column with the engine's type, and a
// field. It returns the quoted table name.
func relationTargetTable(t *testing.T, ctx context.Context, host core.Host, schema string) string {
	t.Helper()
	d := host.Dialect()
	table := core.QuoteIdentifier(d, core.TableName(schema))
	ddl := "CREATE TABLE " + table + " (id UUID PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '', name TEXT)"
	switch d {
	case "mysql":
		ddl = "CREATE TABLE " + table + " (id CHAR(36) PRIMARY KEY, tenant_id VARCHAR(64) NOT NULL DEFAULT '', name TEXT)"
	case "mssql":
		ddl = "CREATE TABLE " + table + " (id UNIQUEIDENTIFIER PRIMARY KEY, tenant_id NVARCHAR(64) NOT NULL DEFAULT '', name NVARCHAR(200))"
	}
	_, err := host.Querier(ctx).Exec(ctx, ddl)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = host.Querier(context.Background()).Exec(context.Background(), "DROP TABLE "+table) })
	return table
}

func relationTargetRow(t *testing.T, ctx context.Context, host core.Host, table, tenantID string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := host.Querier(ctx).Exec(ctx, "INSERT INTO "+table+" (id, tenant_id, name) VALUES ($1, $2, $3)", id.String(), tenantID, "row")
	require.NoError(t, err)
	return id
}

func TestCheckRelationTargets_HoldsATargetToTheTenant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		d := host.Dialect()
		schema := "rt_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		table := relationTargetTable(t, ctx, host, schema)
		ours := relationTargetRow(t, ctx, host, table, "t1")
		theirs := relationTargetRow(t, ctx, host, table, "t2")
		// A row stored under another spelling of the tenant. MySQL and SQL
		// Server compare the column without regard to case, so the
		// statement alone would hand it to t1.
		spelled := relationTargetRow(t, ctx, host, table, "T1")
		check := func(v any) error {
			return core.CheckRelationTargets(ctx, host.Querier(ctx), d, "t1",
				[]core.RelationValue{{Field: "author", Target: schema, Value: v}})
		}

		require.NoError(t, check(ours.String()))
		require.NoError(t, check(strings.ToUpper(ours.String())), "an id is the same id in either case")
		require.NoError(t, check(nil), "a null clears the relation")

		refusals := map[string]any{
			"another tenant's row":         theirs.String(),
			"a row nobody holds":           uuid.NewString(),
			"a row under another spelling": spelled.String(),
			"a value that is not an id":    "not-an-id",
			"an empty value":               "",
		}
		var messages []string
		for name, v := range refusals {
			t.Run(name, func(t *testing.T) {
				err := check(v)
				var refused *core.RelationTargetError
				require.True(t, errors.As(err, &refused), "%v", err)
				assert.Equal(t, "author", refused.Field)
				assert.True(t, errors.Is(err, core.ErrValidation))
				messages = append(messages, err.Error())
			})
		}
		for _, m := range messages {
			assert.Equal(t, messages[0], m, "every refusal reads the same")
		}

		t.Run("a target table that does not exist", func(t *testing.T) {
			err := core.CheckRelationTargets(ctx, host.Querier(ctx), d, "t1",
				[]core.RelationValue{{Field: "ghost", Target: "rt_no_such_table", Value: ours.String()}})
			var refused *core.RelationTargetError
			require.True(t, errors.As(err, &refused), "%v", err)
			assert.Equal(t, "ghost", refused.Field)
		})

		t.Run("the first refused value of several is named", func(t *testing.T) {
			err := core.CheckRelationTargets(ctx, host.Querier(ctx), d, "t1", []core.RelationValue{
				{Field: "author", Target: schema, Value: ours.String()},
				{Field: "editor", Target: schema, Value: theirs.String()},
			})
			var refused *core.RelationTargetError
			require.True(t, errors.As(err, &refused), "%v", err)
			assert.Equal(t, "editor", refused.Field)
		})
	})
}

// More ids than one statement names are read in batches, and every one of
// them is checked.
func TestCheckRelationTargets_ReadsLongListsInBatches(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		d := host.Dialect()
		schema := "rtb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		table := relationTargetTable(t, ctx, host, schema)
		const n = 1205
		values := make([]core.RelationValue, 0, n)
		for start := 0; start < n; start += 200 {
			end := min(start+200, n)
			var marks []string
			var args []any
			for i := start; i < end; i++ {
				id := uuid.New()
				marks = append(marks, fmt.Sprintf("($%d, $%d)", len(args)+1, len(args)+2))
				args = append(args, id.String(), "t1")
				values = append(values, core.RelationValue{Field: "f", Target: schema, Value: id})
			}
			_, err := host.Querier(ctx).Exec(ctx, "INSERT INTO "+table+" (id, tenant_id) VALUES "+strings.Join(marks, ", "), args...)
			require.NoError(t, err)
		}
		require.NoError(t, core.CheckRelationTargets(ctx, host.Querier(ctx), d, "t1", values))

		values[n-1] = core.RelationValue{Field: "last", Target: schema, Value: uuid.New()}
		err := core.CheckRelationTargets(ctx, host.Querier(ctx), d, "t1", values)
		var refused *core.RelationTargetError
		require.True(t, errors.As(err, &refused), "%v", err)
		assert.Equal(t, "last", refused.Field, "a missing id in the last batch is still found")
	})
}

// Inside a transaction the check holds what it read: until the transaction
// ends, no other session can delete a target or move it to another tenant,
// so the write that follows the check cannot point at a row that has left.
func TestCheckRelationTargets_LocksTheTargetsUntilTheTransactionEnds(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		d := host.Dialect()
		schema := "rtl_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		table := relationTargetTable(t, ctx, host, schema)
		target := relationTargetRow(t, ctx, host, table, "t1")

		other, err := host.RawDB().Conn(ctx)
		require.NoError(t, err)
		defer other.Close()
		timeout := map[string]string{
			"postgres": "SET lock_timeout = '300ms'",
			"mysql":    "SET SESSION innodb_lock_wait_timeout = 1",
			"mssql":    "SET LOCK_TIMEOUT 300",
		}[d]
		_, err = other.ExecContext(ctx, timeout)
		require.NoError(t, err)
		attempts := map[string]string{
			"move":   "UPDATE " + table + " SET tenant_id = 't2' WHERE id = '" + target.String() + "'",
			"delete": "DELETE FROM " + table + " WHERE id = '" + target.String() + "'",
		}

		tx, err := host.Querier(ctx).Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, core.CheckRelationTargets(ctx, tx, d, "t1",
			[]core.RelationValue{{Field: "author", Target: schema, Value: target}}))
		for name, stmt := range attempts {
			_, err := other.ExecContext(ctx, stmt)
			assert.Error(t, err, "the %s waits for the transaction that checked the row", name)
		}
		require.NoError(t, tx.Rollback(ctx))

		_, err = other.ExecContext(ctx, attempts["move"])
		require.NoError(t, err, "the lock ends with the transaction")
	})
}
