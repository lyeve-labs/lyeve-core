package db_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

const tenantLocalHash = "$2a$10$placeholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

// One address makes an account in each tenant whose provider asserts it, and
// each is found by its tenant plus the address, never by the install-wide
// email.
func TestCreateTenantLocal_OneAddressInTwoTenants(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		address := "Person-" + uuid.NewString()[:8] + "@Agency.Example"
		folded := strings.ToLower(address)

		a, created, err := users.CreateTenantLocal(ctx, "acme", address, "", nil, true)
		require.NoError(t, err)
		assert.True(t, created)
		assert.Equal(t, core.TenantLocalEmail(a.ID), a.Email, "the stored email is the key")
		assert.Equal(t, folded, a.ProviderEmail, "the address is stored folded")
		require.NotNil(t, a.ProviderSeenAt)
		assert.WithinDuration(t, time.Now(), *a.ProviderSeenAt, time.Minute)
		assert.Equal(t, []string{"editor"}, a.Roles)

		b, created, err := users.CreateTenantLocal(ctx, "brand", address, "", []string{"viewer"}, false)
		require.NoError(t, err)
		assert.True(t, created, "another tenant holding the address decides nothing")
		assert.NotEqual(t, a.ID, b.ID)
		assert.Nil(t, b.ProviderSeenAt, "no provider vouches for it")

		_, err = users.GetByEmail(ctx, address)
		assert.ErrorIs(t, err, domain.ErrNotFound, "the install-wide email never reaches a tenant-local account")

		got, err := users.GetByTenantProviderEmail(ctx, "acme", address)
		require.NoError(t, err)
		assert.Equal(t, a.ID, got.ID)
		got, err = users.GetByTenantProviderEmail(ctx, "brand", folded)
		require.NoError(t, err)
		assert.Equal(t, b.ID, got.ID)
		_, err = users.GetByTenantProviderEmail(ctx, "other", address)
		assert.ErrorIs(t, err, domain.ErrNotFound)

		again, created, err := users.CreateTenantLocal(ctx, "acme", address, "", nil, true)
		require.NoError(t, err, "a second insert for the tenant and address reads the first back")
		assert.False(t, created)
		assert.Equal(t, a.ID, again.ID)

		_, err = users.Create(ctx, address, tenantLocalHash, nil, "acme")
		assert.ErrorIs(t, err, domain.ErrConflict, "a tenant never holds two accounts for one address")
		pw, err := users.Create(ctx, address, tenantLocalHash, nil, "third")
		require.NoError(t, err, "a tenant without a tenant-local account for the address may create one")

		found, err := users.GetByTenantAddress(ctx, "third", address)
		require.NoError(t, err)
		assert.Equal(t, pw.ID, found.ID, "the tenant's own password account is found by email")
		found, err = users.GetByTenantAddress(ctx, "acme", address)
		require.NoError(t, err)
		assert.Equal(t, a.ID, found.ID)
		_, err = users.GetByTenantAddress(ctx, "other", address)
		assert.ErrorIs(t, err, domain.ErrNotFound)

		require.NoError(t, users.StampProviderSeen(ctx, "brand", b.ID))
		stamped, err := users.GetByID(ctx, b.ID)
		require.NoError(t, err)
		require.NotNil(t, stamped.ProviderSeenAt)
		assert.WithinDuration(t, time.Now(), *stamped.ProviderSeenAt, time.Minute)
		assert.ErrorIs(t, users.StampProviderSeen(ctx, "acme", b.ID), domain.ErrNotFound, "a stamp names the account's own tenant")

		for i := 0; i < 2; i++ {
			_, err := users.Create(ctx, "plain-"+uuid.NewString()[:8]+"@agency.example", tenantLocalHash, nil, "fourth")
			require.NoError(t, err, "accounts without a provider address never collide in one tenant")
		}
	})
}

// Two first sign-ins on one tenant and address at the same moment create one
// account, and both callers get it.
func TestCreateTenantLocal_ConcurrentFirstSignInsCreateOneAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-dialect integration test in short mode")
	}
	const callers = 8
	dialects := []struct {
		name string
		open func(*testing.T, int) db.DB
	}{
		{"postgres", testdb.PostgresWithMaxConns},
		{"mysql", testdb.MySQLWithMaxConns},
		{"mssql", testdb.MSSQLWithMaxConns},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.open(t, callers+2)
			users := db.NewUserStore(pool)
			address := "race-" + uuid.NewString()[:8] + "@agency.example"

			start := make(chan struct{})
			var wg sync.WaitGroup
			ids := make([]uuid.UUID, callers)
			made := make([]bool, callers)
			errs := make([]error, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					u, created, err := users.CreateTenantLocal(context.Background(), "acme", address, "", nil, true)
					errs[i], made[i] = err, created
					if u != nil {
						ids[i] = u.ID
					}
				}(i)
			}
			close(start)
			wg.Wait()

			createdCount := 0
			for i := 0; i < callers; i++ {
				require.NoError(t, errs[i], "caller %d", i)
				assert.Equal(t, ids[0], ids[i], "caller %d got another account", i)
				if made[i] {
					createdCount++
				}
			}
			assert.Equal(t, 1, createdCount, "exactly one caller created the account")

			row, err := pool.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM sys_users WHERE tenant_id = $1 AND provider_email = $2`, "acme", address)
			require.NoError(t, err)
			var n int
			require.NoError(t, row.Scan(&n))
			assert.Equal(t, 1, n)
		})
	}
}

// Migration 011 reverses cleanly and runs again over a database that holds it,
// and on SQL Server its unique index is the filtered one.
func TestMigration011_DownUpAndAgain(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		dir := map[string]string{"postgres": "psql", "mysql": "mysql", "mssql": "mssql"}[pool.Engine()]
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join("..", "..", "migrations", dir, name))
			require.NoError(t, err)
			return string(b)
		}
		up, down := read("011_sys_users_provider_email.up.sql"), read("011_sys_users_provider_email.down.sql")
		sqlDB := pool.SQLDB()
		ctx := context.Background()

		_, err := sqlDB.ExecContext(ctx, up)
		require.NoError(t, err, "the pool arrives migrated, so this is the second run")
		_, err = sqlDB.ExecContext(ctx, down)
		require.NoError(t, err)
		assert.Equal(t, 0, providerColumns(t, pool), "down leaves no column behind")
		_, err = sqlDB.ExecContext(ctx, down)
		require.NoError(t, err, "down runs again over a database without the columns")
		_, err = sqlDB.ExecContext(ctx, up)
		require.NoError(t, err)
		assert.Equal(t, 2, providerColumns(t, pool))

		if pool.Engine() == "mssql" {
			var filtered bool
			require.NoError(t, sqlDB.QueryRowContext(ctx,
				`SELECT has_filter FROM sys.indexes WHERE name = 'uq_sys_users_tenant_provider_email' AND object_id = OBJECT_ID('sys_users') AND is_unique = 1`).Scan(&filtered))
			assert.True(t, filtered, "SQL Server needs the filter to admit many rows without a provider address")
		}

		users := db.NewUserStore(pool)
		address := "again-" + uuid.NewString()[:8] + "@agency.example"
		first, _, err := users.CreateTenantLocal(ctx, "acme", address, "", nil, true)
		require.NoError(t, err)
		second, created, err := users.CreateTenantLocal(ctx, "acme", address, "", nil, true)
		require.NoError(t, err)
		assert.False(t, created, "the recreated index holds the tenant and address unique")
		assert.Equal(t, first.ID, second.ID)
	})
}

func providerColumns(t *testing.T, pool db.DB) int {
	t.Helper()
	var q string
	switch pool.Engine() {
	case "mssql":
		q = `SELECT COUNT(*) FROM sys.columns WHERE object_id = OBJECT_ID('sys_users') AND name IN ('provider_email', 'provider_seen_at')`
	case "mysql":
		q = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND column_name IN ('provider_email', 'provider_seen_at')`
	default:
		q = `SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'sys_users' AND column_name IN ('provider_email', 'provider_seen_at')`
	}
	var n int
	require.NoError(t, pool.SQLDB().QueryRowContext(context.Background(), q).Scan(&n))
	return n
}
