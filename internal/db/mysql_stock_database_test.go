package db_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

func coreMigrations(t *testing.T) string {
	t.Helper()
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "migrations")
}

func schemaCollation(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var collation string
	require.NoError(t, conn.QueryRow("SELECT @@collation_database").Scan(&collation))
	return collation
}

// A stock MySQL 8 database and the URL the docs print, with no driver
// parameters, are the setup an operator starts from. The engine has to migrate
// it and read times from it without the operator adding anything.
func TestMigrate_StockMySQLDatabaseWorksAsDocumented(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql dialect not enabled")
	}
	ctx := context.Background()
	url := testdb.MySQLStockDatabase(t)

	_, err := db.Migrate(url, coreMigrations(t), nil)
	require.NoError(t, err, "a stock database must migrate")

	d, err := db.Connect(ctx, url, 2)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	conn := d.SQLDB()

	assert.Equal(t, "utf8mb4_unicode_ci", schemaCollation(t, conn),
		"the engine gives a new database the collation its plugins' tables name")
	var tableCollation string
	require.NoError(t, conn.QueryRow(
		"SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'sys_users'").Scan(&tableCollation))
	assert.Equal(t, "utf8mb4_unicode_ci", tableCollation)

	var now time.Time
	require.NoError(t, conn.QueryRow("SELECT NOW()").Scan(&now),
		"a URL without parseTime must still read times")
	assert.False(t, now.IsZero())
}

// The lock name moved from the server to the engine because MySQL 9 has no
// MD5 function. An engine still asking the server must take the same lock.
func TestMySQLSchemaLockName_MatchesTheServersMD5(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql dialect not enabled")
	}
	ctx := context.Background()
	d, err := db.Connect(ctx, testdb.MySQLStockDatabase(t), 2)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	conn, err := d.SQLDB().Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	var fromServer string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT CONCAT('lyeve_migration_', MD5(DATABASE()))").Scan(&fromServer))
	got, err := db.MySQLSchemaLockName(ctx, conn, "lyeve_migration")
	require.NoError(t, err)
	assert.Equal(t, fromServer, got)
}

// Changing the default of a database whose tables already exist would leave
// the old tables in one collation and the new ones in another, so the engine
// leaves an installed database as it found it.
func TestEnsureMySQLCollation_LeavesAnInstalledDatabaseAlone(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql dialect not enabled")
	}
	ctx := context.Background()
	d, err := db.Connect(ctx, testdb.MySQLStockDatabase(t), 2)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	conn, err := d.SQLDB().Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.ExecContext(ctx, "CREATE TABLE sys_users (id CHAR(36) PRIMARY KEY)")
	require.NoError(t, err)
	require.NoError(t, db.EnsureMySQLCollation(ctx, conn))
	assert.Equal(t, "utf8mb4_0900_ai_ci", schemaCollation(t, d.SQLDB()))
}
