package plugintest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The catalog helpers see only the test's own database. Two tests share one
// server per dialect, and on MySQL information_schema lists every database on
// it, so a table another test created must not count as this test's.
func TestCatalogHelpers_SeeOnlyTheirOwnDatabase(t *testing.T) {
	hosts := map[string]func(*testing.T) core.Host{
		"postgres": plugintest.Postgres,
		"mysql":    plugintest.MySQL,
		"mssql":    plugintest.MSSQL,
	}
	for name, newHost := range hosts {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			own, other := newHost(t), newHost(t)
			_, err := other.Querier(ctx).Exec(ctx, `CREATE TABLE catalog_scope_probe (id INT)`)
			require.NoError(t, err)

			assert.False(t, plugintest.NewDBFixture(t, own).TableExists(ctx, "catalog_scope_probe"),
				"a table in another test's database counted as this one's")
			assert.True(t, plugintest.NewDBFixture(t, other).TableExists(ctx, "catalog_scope_probe"))
			plugintest.AssertTablesExist(t, ctx, other, "catalog_scope_probe")
			plugintest.AssertMigrationTable(t, ctx, other, "catalog_scope_probe")
		})
	}
}
