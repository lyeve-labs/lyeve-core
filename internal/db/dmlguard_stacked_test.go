package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// A query can carry several statements, and the guard has to read each one.
// Text inside a string, a quoted identifier or a comment starts no statement,
// and text the engine's server would run is never read as quoted.
func TestIsDMLOn_ReadsEveryStatement(t *testing.T) {
	for _, tc := range []struct {
		engine, query string
		dml           bool
	}{
		{"postgres", "SELECT 1; DELETE FROM sys_users", true},
		{"mysql", "SELECT 1;\n  /* then */ UPDATE sys_users SET email = ''", true},
		{"mssql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1)", true},
		{"postgres", "SELECT ';DELETE FROM sys_users'", false},
		{"postgres", "SELECT 1 -- ; DELETE FROM sys_users\n", false},
		{"postgres", "SELECT $$;DELETE FROM sys_users$$", false},
		{"postgres", "SELECT $tag$';$tag$; DELETE FROM sys_users", true},
		{"postgres", "SELECT E'\\''; DELETE FROM sys_users", true},
		{"postgres", "SELECT 'a\\'; DELETE FROM sys_users; SELECT ''", true},
		{"postgres", "SELECT \"x;y\" FROM t", false},
		{"postgres", "SELECT a$b$c; DELETE FROM sys_users", true},
		{"mysql", "SELECT 'it\\'s'; DELETE FROM sys_users", true},
		{"mysql", "SELECT 'it\\';DELETE FROM sys_users'", false},
		{"mysql", "SELECT `a;b` FROM t", false},
		{"mysql", "SELECT 1 # ; DELETE\n", false},
		{"mysql", "SELECT 1 --; DELETE FROM sys_users", true},
		{"mysql", "SELECT 1 /*!; DELETE FROM sys_users */", true},
		{"mssql", "SELECT [a;b] FROM t", false},
		{"mssql", "SELECT N'x;DELETE FROM t'", false},
		{"postgres", "CREATE TABLE t (id INT REFERENCES u (id) ON DELETE CASCADE)", false},
		{"postgres", "/* purge */ DELETE FROM users WHERE id = 1", true},
	} {
		assert.Equal(t, tc.dml, db.IsDMLOn(tc.engine, tc.query), "%s: %q", tc.engine, tc.query)
	}
}

// The probe the guard was found open to: a read followed by a delete in one
// Exec. Each server runs the second statement when the guard lets it through.
func TestDMLGuard_RefusesAStackedStatement(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "mssql"} {
		t.Run(engine, func(t *testing.T) {
			if !testdb.ShouldTest(engine) {
				t.Skipf("%s dialect not enabled", engine)
			}
			ctx := context.Background()
			var (
				pool db.DB
				dsn  string
			)
			switch engine {
			case "mysql":
				pool, dsn = testdb.MySQLWithDSN(t)
			case "mssql":
				pool, dsn = testdb.MSSQLWithDSN(t)
			default:
				pool, dsn = testdb.PostgresWithDSN(t)
			}
			table := "guard_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
			_, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id INT PRIMARY KEY)`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO `+table+` (id) VALUES (1)`)
			require.NoError(t, err)

			guard, err := db.NewDMLGuardDB(ctx, engine, dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = guard.Close() })

			_, err = guard.ExecContext(ctx, `SELECT 1; DELETE FROM `+table)
			require.ErrorIs(t, err, db.ErrDMLRejected)

			var n int
			require.NoError(t, guard.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
			require.Equal(t, 1, n, "the stacked delete ran")
		})
	}
}
