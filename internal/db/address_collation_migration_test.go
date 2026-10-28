package db_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// collatedPairs are pairs of different addresses that a folding collation
// compares as one value. SQL Server's default collation is accent-sensitive,
// so it keeps the accent pair apart even before migration 012.
var collatedPairs = []struct {
	name, stored, other string
	mssqlFolds          bool
}{
	{"accent", "josé", "jose", false},
	{"sharp s", "straße", "strasse", true},
	{"ligature", "ﬁ", "fi", true},
}

// Before migration 012 the unique keys on MySQL and SQL Server refuse an
// address that the folding collation compares as equal to one an account
// holds. After it each such address is an account of its own, install-wide
// and tenant-local, while an address that differs only by case still names
// the existing account. The migration reverses, and each direction runs again
// over a database that already holds it.
func TestMigration012_DistinctAddressesBecomeDistinctAccounts(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		if pool.Engine() == "postgres" {
			t.Skip("PostgreSQL compares addresses by byte, and the migration changes nothing there")
		}
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join("..", "..", "migrations", pool.Engine(), name))
			require.NoError(t, err)
			return string(b)
		}
		up, down := read("012_sys_users_address_binary_collation.up.sql"), read("012_sys_users_address_binary_collation.down.sql")
		ctx := context.Background()
		sqlDB := pool.SQLDB()
		users := db.NewUserStore(pool)

		for range 2 {
			_, err := sqlDB.ExecContext(ctx, down)
			require.NoError(t, err)
		}
		for _, pair := range collatedPairs {
			folds := pool.Engine() == "mysql" || pair.mssqlFolds
			suffix := "-" + uuid.NewString()[:8] + "@example.com"
			stored, other := pair.stored+suffix, pair.other+suffix

			_, err := users.Create(ctx, stored, tenantLocalHash, nil, "acme")
			require.NoError(t, err)
			_, err = users.Create(ctx, other, tenantLocalHash, nil, "acme")
			assert.Equal(t, folds, err != nil, "before, %s: the unique key on email refused %s: %v", pair.name, other, err)

			local, created, err := users.CreateTenantLocal(ctx, "brand", stored, "", nil, true)
			require.NoError(t, err)
			require.True(t, created)
			second, created, err := users.CreateTenantLocal(ctx, "brand", other, "", nil, true)
			madeSecond := err == nil && created && second.ID != local.ID
			assert.Equal(t, !folds, madeSecond, "before, %s: a second tenant-local account for %s: %v", pair.name, other, err)
		}

		for range 2 {
			_, err := sqlDB.ExecContext(ctx, up)
			require.NoError(t, err)
		}
		for _, pair := range collatedPairs {
			suffix := "-" + uuid.NewString()[:8] + "@example.com"
			stored, other := pair.stored+suffix, pair.other+suffix

			first, err := users.Create(ctx, stored, tenantLocalHash, nil, "acme")
			require.NoError(t, err)
			second, err := users.Create(ctx, other, tenantLocalHash, nil, "acme")
			require.NoError(t, err, "after, %s: the unique key on email refused %s", pair.name, other)
			assert.NotEqual(t, first.ID, second.ID)
			_, err = users.Create(ctx, pair.stored+strings.ToUpper(suffix), tenantLocalHash, nil, "acme")
			assert.Error(t, err, "after, %s: an address differing only by case made a second account", pair.name)

			local, created, err := users.CreateTenantLocal(ctx, "brand", stored, "", nil, true)
			require.NoError(t, err)
			require.True(t, created)
			otherLocal, created, err := users.CreateTenantLocal(ctx, "brand", other, "", nil, true)
			require.NoError(t, err, "after, %s: tenant-local %s", pair.name, other)
			assert.True(t, created, "after, %s: tenant-local %s was not created", pair.name, other)
			assert.NotEqual(t, local.ID, otherLocal.ID)
			again, created, err := users.CreateTenantLocal(ctx, "brand", pair.stored+strings.ToUpper(suffix), "", nil, true)
			require.NoError(t, err)
			assert.False(t, created, "after, %s: an address differing only by case made a second tenant-local account", pair.name)
			assert.Equal(t, local.ID, again.ID)
		}
	})
}
