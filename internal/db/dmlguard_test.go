package db_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The side pools take DATABASE_URL as the operator wrote it. In the documented
// mysql:// form the driver reads the scheme as the user name, so a pool that
// hands the URL over unconverted signs in as "mysql" and the engine runs
// without its DML guard and its lock pool.
func TestGuardedPools_OpenFromTheDocumentedMySQLURL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql dialect not enabled")
	}
	ctx := context.Background()
	url := testdb.MySQLStockDatabase(t)

	for name, open := range map[string]func(context.Context, string, string) (*sql.DB, error){
		"dml guard": db.NewDMLGuardDB,
		"locks":     db.NewLockDB,
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := open(ctx, "mysql", url)
			require.NoError(t, err, "the pool must open from a mysql:// URL")
			t.Cleanup(func() { _ = pool.Close() })

			var now time.Time
			require.NoError(t, pool.QueryRowContext(ctx, "SELECT NOW()").Scan(&now),
				"the pool reads times the way the main pool does")
			assert.False(t, now.IsZero())

			_, err = pool.ExecContext(ctx, "DELETE FROM sys_users")
			assert.ErrorIs(t, err, db.ErrDMLRejected)
		})
	}
}
