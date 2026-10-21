package core_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A tenant-scoped PostgreSQL connection searches the tenant's schema first and
// the engine's schema after it. An unqualified engine table still resolves on
// it, so the check has to find the table there too, or a guard on it skips
// its work as if the table had never been created.
func TestTableSetCheck_FindsATableTheSearchPathResolves(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		if host.Dialect() != "postgres" {
			t.Skip("the search path is PostgreSQL's")
		}
		ctx := context.Background()
		q := host.Querier(ctx)
		suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
		table, tenantSchema := "sys_resolved_"+suffix, "tenant_resolved_"+suffix
		_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })
		_, err = q.Exec(ctx, `CREATE SCHEMA `+tenantSchema)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP SCHEMA `+tenantSchema+` CASCADE`) })

		tx, err := q.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		row, err := tx.QueryRow(ctx, `SELECT current_schema()`)
		require.NoError(t, err)
		var engineSchema string
		require.NoError(t, row.Scan(&engineSchema))
		_, err = tx.Exec(ctx, `SET LOCAL search_path = `+tenantSchema+`, `+engineSchema)
		require.NoError(t, err)

		set := core.NewTableSet([]string{table}, []string{table + ".tenant_id"})
		state, err := set.Check(ctx, tx, host.Dialect())
		require.NoError(t, err)
		require.Equal(t, core.TableSetReady, state, "the check read only the tenant's schema")
	})
}
