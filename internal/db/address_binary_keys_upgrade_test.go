package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// binaryKeyDialects are the dialects whose address keys migration 012 changes.
var binaryKeyDialects = []struct {
	name string
	open func(*testing.T) (db.DB, string)
}{
	{"mysql", testdb.MySQLWithDSN},
	{"mssql", testdb.MSSQLWithDSN},
}

// rollBackTo runs the engine's down migrations until the database holds
// version, and records it.
func rollBackTo(t *testing.T, pool db.DB, version int) {
	t.Helper()
	ctx := context.Background()
	for v := 12; v > version; v-- {
		found, err := filepath.Glob(filepath.Join("..", "..", "migrations", pool.Engine(), fmt.Sprintf("%03d_*.down.sql", v)))
		require.NoError(t, err)
		require.Len(t, found, 1)
		down, err := os.ReadFile(found[0])
		require.NoError(t, err)
		_, err = pool.SQLDB().ExecContext(ctx, string(down))
		require.NoError(t, err, found[0])
	}
	_, err := pool.SQLDB().ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET version = %d`, db.MigrationsTable, version))
	require.NoError(t, err)
}

// decomposedAccount stores an account whose address is in decomposed form, as
// a row written before addresses were stored in one form, and returns its id
// and the composed form of the address.
func decomposedAccount(t *testing.T, pool db.DB) (string, string) {
	t.Helper()
	composed := "josé-" + uuid.NewString()[:8] + "@example.com"
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id) VALUES ($1, $2, $3, $4, $5)`,
		id, norm.NFD.String(composed), "", `["editor"]`, "acme")
	require.NoError(t, err)
	return id, composed
}

// An upgrade that crosses migration 012 rewrites a decomposed address before
// the keys compare by byte, from version 11 and from older schemas alike. The
// account is then found by the composed form, and a sign-up in that form is
// refused by the unique key.
func TestMigrate_UpgradeRewritesDecomposedAddressesBeforeBinaryKeys(t *testing.T) {
	for _, d := range binaryKeyDialects {
		for _, from := range []int{10, 11} {
			t.Run(fmt.Sprintf("%s/from_%d", d.name, from), func(t *testing.T) {
				if !testdb.ShouldTest(d.name) {
					t.Skipf("CI_DIALECT != %s", d.name)
				}
				ctx := context.Background()
				pool, dsn := d.open(t)
				rollBackTo(t, pool, from)
				id, composed := decomposedAccount(t, pool)

				n, err := db.Migrate(dsn, filepath.Join("..", "..", "migrations"), nil)
				require.NoError(t, err)
				require.Equal(t, 12-from, n)

				users := db.NewUserStore(pool)
				got, err := users.GetByEmail(ctx, composed)
				require.NoError(t, err, "the account is found by the composed form")
				assert.Equal(t, id, got.ID.String())
				_, err = users.Create(ctx, composed, "", nil, "acme")
				assert.True(t, core.IsDuplicateKey(err), "a sign-up in the composed form: %v", err)
			})
		}
	}
}

// Two boots of one database at once both succeed, and the database ends at
// version 12, clean, with the one account rewritten.
func TestMigrate_ConcurrentBootsRewriteBeforeBinaryKeys(t *testing.T) {
	for _, d := range binaryKeyDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			ctx := context.Background()
			pool, dsn := d.open(t)
			rollBackTo(t, pool, 10)
			id, composed := decomposedAccount(t, pool)

			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i := range errs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = db.Migrate(dsn, filepath.Join("..", "..", "migrations"), nil)
				}()
			}
			wg.Wait()
			for i, err := range errs {
				require.NoError(t, err, "boot %d", i)
			}

			var version int64
			var dirty bool
			require.NoError(t, pool.SQLDB().QueryRowContext(ctx, `SELECT version, dirty FROM `+db.MigrationsTable).Scan(&version, &dirty))
			assert.Equal(t, int64(12), version)
			assert.False(t, dirty)

			got, err := db.NewUserStore(pool).GetByEmail(ctx, composed)
			require.NoError(t, err, "the account is found by the composed form")
			assert.Equal(t, id, got.ID.String())
			row, err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sys_users WHERE email = $1`, norm.NFD.String(composed))
			require.NoError(t, err)
			var left int
			require.NoError(t, row.Scan(&left))
			assert.Zero(t, left, "rows left in decomposed form")
		})
	}
}
