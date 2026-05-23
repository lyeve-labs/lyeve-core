package plugin

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"  // MySQL driver for the lock tests
	_ "github.com/jackc/pgx/v5/stdlib"  // Postgres driver for the lock tests
	_ "github.com/microsoft/go-mssqldb" // MSSQL driver for the lock tests
	"github.com/stretchr/testify/require"
)

func lockTestDB(t *testing.T, envKey, dialect string) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv(envKey)
	if dsn == "" {
		t.Skipf("%s not set", envKey)
	}
	driver := map[string]string{"postgres": "pgx", "mysql": "mysql", "mssql": "sqlserver"}[dialect]
	db, err := sql.Open(driver, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db, dialect
}

// The lock is session-scoped and sql.Conn.Close only returns the connection to
// the pool, so without an explicit release the session keeps holding it. The
// next caller can then be handed that same pooled connection and wait out the
// full timeout against itself.
func TestMigrationLock_ReleasesSoTheNextCallerCanAcquire(t *testing.T) {
	for _, tc := range []struct{ env, dialect string }{
		{"TEST_POSTGRES_DSN", "postgres"},
		{"TEST_MYSQL_DSN", "mysql"},
		{"TEST_MSSQL_DSN", "mssql"},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			db, dialect := lockTestDB(t, tc.env, tc.dialect)
			// A single connection makes the pool hand the second caller the
			// very connection the first one used.
			db.SetMaxOpenConns(1)

			ctx := context.Background()
			for i := 0; i < 3; i++ {
				start := time.Now()
				release, err := acquireMigrationLock(ctx, db, dialect)
				require.NoErrorf(t, err, "acquire %d", i)
				require.Lessf(t, time.Since(start), 5*time.Second,
					"acquire %d waited on a lock nobody was holding", i)
				release()
			}
		})
	}
}

// A second holder must wait rather than proceed, or the serialization the
// migration loop depends on is not there.
func TestMigrationLock_IsExclusive(t *testing.T) {
	db, dialect := lockTestDB(t, "TEST_POSTGRES_DSN", "postgres")
	db.SetMaxOpenConns(4)
	ctx := context.Background()

	release, err := acquireMigrationLock(ctx, db, dialect)
	require.NoError(t, err)

	held := make(chan struct{})
	go func() {
		r2, err2 := acquireMigrationLock(ctx, db, dialect)
		if err2 == nil {
			r2()
		}
		close(held)
	}()

	select {
	case <-held:
		release()
		t.Fatal("second caller acquired while the lock was held")
	case <-time.After(3 * migrationLockPoll):
	}

	release()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("second caller never acquired after release")
	}
}

func TestMigrationLock_UnknownDialectIsANoop(t *testing.T) {
	release, err := acquireMigrationLock(context.Background(), nil, "cockroach")
	require.NoError(t, err)
	require.NotNil(t, release)
	release()
}

// lockSQLFor and unlockSQLFor must key on the same resource or the release is
// a silent no-op.
func TestMigrationLock_LockAndUnlockAgreeOnDialects(t *testing.T) {
	for _, d := range []string{"postgres", "postgresql", "mysql", "mssql", "sqlserver"} {
		require.NotEmptyf(t, lockSQLFor(d), "no lock statement for %s", d)
		require.NotEmptyf(t, unlockSQLFor(d), "no unlock statement for %s", d)
	}
	require.Empty(t, lockSQLFor("cockroach"))
	require.Empty(t, unlockSQLFor("cockroach"))
}
