//go:build !short && !mutest

package enginehost

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// A tenant-scoped MySQL or SQL Server connection has switched to the tenant's
// database, and a statement on it reaches an engine table in the engine's
// database. The table check reads an engine table there, and reads any other
// table in the database the connection is on, the one a statement reaches.
func TestTableSetCheck_TenantConnReadsEngineTablesInTheEngineDatabase(t *testing.T) {
	for _, name := range []string{"mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			var pool db.DB
			quote := func(s string) string { return "`" + s + "`" }
			if name == "mssql" {
				pool = testdb.MSSQL(t)
				quote = func(s string) string { return "[" + s + "]" }
			} else {
				pool = testdb.MySQL(t)
			}
			ctx := context.Background()
			h := NewHost(pool, pool.SQLDB(), &config.Config{MultiTenant: true}, hooks.NewRegistry(), "test").(*engineHost)

			suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
			engineTable, ownTable := "sys_resolved_"+suffix, "resolved_"+suffix
			q := h.Querier(ctx)
			for _, table := range []string{engineTable, ownTable} {
				_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
				require.NoError(t, err)
				t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE `+table) })
			}
			slug := "resolved" + suffix
			tenantDB := sqlx.TenantSchemaName(slug)
			_, err := pool.Exec(ctx, `CREATE DATABASE `+quote(tenantDB))
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP DATABASE `+quote(tenantDB)) })

			tctx, cleanup, err := h.AcquireTenantConn(ctx, slug)
			require.NoError(t, err)
			defer cleanup()
			require.NotNil(t, db.TenantConn(tctx), "the context carries no tenant connection")

			engineSet := core.NewTableSet([]string{engineTable}, []string{engineTable + ".tenant_id"})
			ownSet := core.NewTableSet([]string{ownTable}, nil)
			tq := h.Querier(tctx)
			tx, err := tq.Begin(tctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(tctx) }()
			for label, on := range map[string]core.Querier{"querier": tq, "transaction": tx} {
				state, err := engineSet.Check(tctx, on, name)
				require.NoError(t, err, label)
				require.Equal(t, core.TableSetReady, state, "%s: the check read the engine table in the tenant's database", label)
				state, err = ownSet.Check(tctx, on, name)
				require.NoError(t, err, label)
				require.Equal(t, core.TableSetAbsent, state, "%s: a statement on this connection does not reach the table", label)
			}
		})
	}
}
