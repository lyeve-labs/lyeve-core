package runtime

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
)

// An install set up without tenants and switched to them later has no
// default row on the roster, while its accounts with no tenant act in the
// default tenant. An all-tenant export still visits the default tenant, on a
// connection scoped through the engine's own path, so those accounts are
// exported once and the bundle is complete.
func TestDSARTenantLister_SweepReachesTheDefaultTenantOffTheRoster(t *testing.T) {
	dialects := map[string]func(*testing.T) db.DB{
		"postgres": testdb.Postgres,
		"mysql":    testdb.MySQL,
		"mssql":    testdb.MSSQL,
	}
	for _, dialect := range []string{"postgres", "mysql", "mssql"} {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			pool := dialects[dialect](t)
			host := enginehost.NewHost(pool, pool.SQLDB(), &config.Config{MultiTenant: true}, nil, "test")

			address := "upgraded-" + uuid.NewString()[:8] + "@agency.example"
			_, err := host.Querier(ctx).Exec(ctx,
				`INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, $3, $4)`,
				uuid.New(), address, "hashed", "")
			require.NoError(t, err)

			compliance.ResetSubjectExporters()
			t.Cleanup(compliance.ResetSubjectExporters)
			registerSysUserExporter(host)

			roster := func(context.Context) ([]string, error) { return []string{"acme"}, nil }
			tenants, err := dsarTenantLister(roster)(ctx)
			require.NoError(t, err)

			result, err := compliance.RunSubjectExportAcrossTenants(ctx, address, tenants, dsarTenantScope(host))
			require.NoError(t, err)
			assert.Equal(t, []string{core.DefaultTenantSlug}, result.MatchedTenants)
			assert.Equal(t, 1, result.Summary.TotalRecords)
			assert.False(t, result.Incomplete, "%#v", result.Errors)
		})
	}
}
