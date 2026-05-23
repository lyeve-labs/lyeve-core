//go:build !mutest

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMySQL_ConnectAndMigrate verifies the full MySQL lifecycle:
//  1. Start a MySQL 8.0 container via testcontainers
//  2. Connect using the db.Connect seam (validates EngineFromDSN + driverNameFor)
//  3. Apply core migrations via db.Migrate
//  4. Verify tables from the init migration exist
//  5. Smoke test with basic CRUD
func TestMySQL_ConnectAndMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MySQL integration test in short mode")
	}

	ctx := context.Background()

	// Start MySQL container

	ctr, err := mysql.Run(ctx,
		containerImage("mysql:8"),
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start mysql container")
	t.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	// Connect through the existing seam

	connStr, err := ctr.ConnectionString(ctx, "parseTime=true", "loc=UTC", "multiStatements=true")
	require.NoError(t, err, "connection string")

	pool, err := Connect(ctx, connStr, 4)
	require.NoError(t, err, "connect to MySQL via db.Connect")
	t.Cleanup(func() { pool.Close() })

	// Verify engine detection.
	require.Equal(t, "mysql", pool.Engine(), "engine should be mysql")

	// Ping to confirm connectivity.
	require.NoError(t, pool.Ping(ctx), "ping MySQL")

	// Run core migrations
	// db.Migrate resolves mysql -> mysql/ subdir in the migrations directory.

	n, err := Migrate(connStr, "../../migrations", nil)
	require.NoError(t, err, "apply MySQL migrations")
	t.Logf("applied %d MySQL migrations", n)

	// Verify migration tracking table exists

	var trackCount int
	row, qrErr := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'engine_schema_migrations' AND table_schema = DATABASE()`,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&trackCount)
	require.NoError(t, err, "query engine_schema_migrations table")
	require.Equal(t, 1, trackCount, "engine_schema_migrations table should exist")

	// Verify tables from 001_init

	// The schema registry and the DDL queue are a plugin's tables. This
	// module's migrations create neither, and the plugin proves its own.
	t.Run("sys_users_exists", func(t *testing.T) {
		assertMySQLTableExists(t, pool, "sys_users")
	})

	// Verify columns in sys_users

	t.Run("columns_sys_users", func(t *testing.T) {
		assertMySQLColumns(t, pool, "sys_users", []string{
			"id", "email", "password_hash", "roles", "created_at", "updated_at",
		})
	})

	// Basic CRUD smoke test on sys_users

	t.Run("crud_sys_users", func(t *testing.T) {
		testEmail := fmt.Sprintf("smoke-%d@test.local", time.Now().UnixNano())

		// INSERT
		_, err := pool.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, created_at, updated_at)
			 VALUES ($1, $2, $3, NOW(6), NOW(6))`,
			"00000000-0000-0000-0000-000000000001",
			testEmail,
			"$2a$10$placeholder-hash-for-smoke-test",
		)
		require.NoError(t, err, "insert smoke row")

		// SELECT
		var gotEmail string
		row, qrErr = pool.QueryRow(ctx,
			`SELECT email FROM sys_users WHERE id = $1`,
			"00000000-0000-0000-0000-000000000001",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&gotEmail)
		require.NoError(t, err, "select smoke row")
		require.Equal(t, testEmail, gotEmail, "email should match")

		// UPDATE
		updatedEmail := fmt.Sprintf("updated-%d@test.local", time.Now().UnixNano())
		result, err := pool.Exec(ctx,
			`UPDATE sys_users SET email = $1 WHERE id = $2`,
			updatedEmail,
			"00000000-0000-0000-0000-000000000001",
		)
		require.NoError(t, err, "update smoke row")
		n, err := result.RowsAffected()
		require.NoError(t, err, "rows affected after update")
		require.Equal(t, int64(1), n, "should update 1 row")

		// Verify update
		row, qrErr = pool.QueryRow(ctx,
			`SELECT email FROM sys_users WHERE id = $1`,
			"00000000-0000-0000-0000-000000000001",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&gotEmail)
		require.NoError(t, err, "select after update")
		require.Equal(t, updatedEmail, gotEmail, "email should be updated")

		// DELETE
		_, err = pool.Exec(ctx,
			`DELETE FROM sys_users WHERE id = $1`,
			"00000000-0000-0000-0000-000000000001",
		)
		require.NoError(t, err, "delete smoke row")

		// Verify deletion
		row, qrErr = pool.QueryRow(ctx,
			`SELECT email FROM sys_users WHERE id = $1`,
			"00000000-0000-0000-0000-000000000001",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&gotEmail)
		require.ErrorIs(t, err, sql.ErrNoRows, "should get ErrNoRows after delete")
	})

	t.Log("MySQL integration: connected, migrated, CRUD verified - PASS")
}

// TestMySQL_DialectRewrite verifies that the placeholder rewrite pipeline
// works correctly against a real MySQL connection. The $N placeholders should
// be rewritten to ? by the sqlDB layer.
func TestMySQL_DialectRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MySQL integration test in short mode")
	}

	ctx := context.Background()

	ctr, err := mysql.Run(ctx,
		containerImage("mysql:8"),
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start mysql container")
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	connStr, err := ctr.ConnectionString(ctx, "parseTime=true", "loc=UTC", "multiStatements=true")
	require.NoError(t, err, "connection string")

	pool, err := Connect(ctx, connStr, 4)
	require.NoError(t, err, "connect to MySQL")
	t.Cleanup(func() { pool.Close() })

	// Single parameter query - $1 -> ?
	row, qrErr := pool.QueryRow(ctx, "SELECT $1", 42)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var result int
	require.NoError(t, row.Scan(&result), "scan parameterized query result")
	require.Equal(t, 42, result, "SELECT $1 should return 42")

	// Multi-parameter query.
	row, qrErr = pool.QueryRow(ctx, "SELECT $1 + $2", 100, 23)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var sum int
	require.NoError(t, row.Scan(&sum), "scan multi-parameter query result")
	require.Equal(t, 123, sum, "100+23 should be 123")

	// String parameter.
	row, qrErr = pool.QueryRow(ctx, "SELECT $1", "hello")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var got string
	require.NoError(t, row.Scan(&got), "scan string parameter")
	require.Equal(t, "hello", got, "string parameter round-trip")
}

// TestMySQL_MigrationsIdempotent verifies that rerunning migrations after
// they've already been applied is safe (no errors, no double-counting).
func TestMySQL_MigrationsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MySQL migration idempotency test in short mode")
	}

	ctx := context.Background()

	ctr, err := mysql.Run(ctx,
		containerImage("mysql:8"),
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start mysql container")
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	connStr, err := ctr.ConnectionString(ctx, "parseTime=true", "loc=UTC", "multiStatements=true")
	require.NoError(t, err, "connection string")

	// First run: migrations from scratch.
	n1, err := Migrate(connStr, "../../migrations", nil)
	require.NoError(t, err, "first migrate")
	t.Logf("first migrate: applied %d migrations", n1)

	// Second run: should return 0 (no new migrations) without error.
	n2, err := Migrate(connStr, "../../migrations", nil)
	require.NoError(t, err, "second migrate (should be idempotent)")
	require.Equal(t, 0, n2, "second migrate should apply 0 migrations")
}

// Helpers

// containerImage resolves the Docker image for MySQL. If CI_IMAGE_MYSQL is set
// it overrides the default, allowing CI matrix jobs to inject different versions.
func containerImage(defaultImage string) string {
	if img := os.Getenv("CI_IMAGE_MYSQL"); img != "" {
		return img
	}
	return defaultImage
}

// assertMySQLTableExists checks that a table exists in the current database.
func assertMySQLTableExists(t *testing.T, pool DB, tableName string) {
	t.Helper()
	var count int
	row, qrErr := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM information_schema.tables
		  WHERE table_name = ? AND table_schema = DATABASE()`,
		tableName,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&count)
	require.NoError(t, err, "query information_schema for %s", tableName)
	if count != 1 {
		t.Errorf("expected table %q to exist (count=%d)", tableName, count)
	}
}

// assertMySQLColumns verifies that the expected columns exist in the given table.
func assertMySQLColumns(t *testing.T, pool DB, tableName string, wantCols []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns
		  WHERE table_name = $1 AND table_schema = DATABASE()
		  ORDER BY ordinal_position`,
		tableName,
	)
	require.NoError(t, err, "query columns for %s", tableName)
	defer rows.Close()

	var gotCols []string
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		gotCols = append(gotCols, col)
	}
	require.NoError(t, rows.Err())

	for _, want := range wantCols {
		found := false
		for _, got := range gotCols {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("column %q not found in table %q; have: [%s]",
				want, tableName, joinComma(gotCols))
		}
	}
}

// joinStrings is provided by migrate_test.go: not redeclared here.
