//go:build !mutest

package testdb_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestMySQLContainer_SpinUp verifies MySQL 8 testcontainer startup, core
// migration application, and basic CRUD.
func TestMySQLContainer_SpinUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MySQL integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is set to a different adapter; skipping MySQL")
	}

	pool := testdb.MySQL(t)

	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping mysql: %v", err)
	}

	// Verify the engine reported matches reality.
	if got := pool.Engine(); got != "mysql" {
		t.Fatalf("pool.Engine() = %q, want %q", got, "mysql")
	}

	// Basic CRUD smoke test

	type user struct {
		ID    string
		Email string
	}

	insertSQL := `INSERT INTO sys_users (id, email, password_hash, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(6), NOW(6))`
	result, err := pool.Exec(ctx, insertSQL,
		"00000000-0000-0000-0000-000000000001",
		"test@example.com",
		"hashed-password",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 1 {
		t.Fatalf("insert affected %d rows, want 1", n)
	}

	selectSQL := `SELECT id, email FROM sys_users WHERE id = $1`
	var u user
	row, qrErr := pool.QueryRow(ctx, selectSQL, "00000000-0000-0000-0000-000000000001")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	if err := row.Scan(&u.ID, &u.Email); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if u.ID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("id = %q", u.ID)
	}
	if u.Email != "test@example.com" {
		t.Errorf("email = %q", u.Email)
	}

	updateSQL := `UPDATE sys_users SET email = $1 WHERE id = $2`
	result, err = pool.Exec(ctx, updateSQL, "updated@example.com", "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("update user: %v", err)
	}
	n, _ = result.RowsAffected()
	if n != 1 {
		t.Fatalf("update affected %d rows, want 1", n)
	}

	row, qrErr = pool.QueryRow(ctx, selectSQL, "00000000-0000-0000-0000-000000000001")
	if qrErr != nil {
		t.Fatalf("QueryRow after update: %v", qrErr)
	}
	if err := row.Scan(&u.ID, &u.Email); err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if u.Email != "updated@example.com" {
		t.Errorf("email after update = %q, want 'updated@example.com'", u.Email)
	}

	deleteSQL := `DELETE FROM sys_users WHERE id = $1`
	_, err = pool.Exec(ctx, deleteSQL, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}

	row, qrErr = pool.QueryRow(ctx, selectSQL, "00000000-0000-0000-0000-000000000001")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	if err := row.Scan(&u.ID, &u.Email); err != sql.ErrNoRows {
		t.Fatalf("expected ErrNoRows after delete, got: %v", err)
	}

	t.Log("MySQL 8 container: spin up, migrations, CRUD (INSERT/SELECT/UPDATE/DELETE)")
}

// TestPostgresContainer_SpinUp verifies Postgres testcontainer startup,
// migration application, and basic CRUD.
func TestPostgresContainer_SpinUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Postgres integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is set to a different adapter; skipping Postgres")
	}

	pool := testdb.Postgres(t)

	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}

	if got := pool.Engine(); got != "postgres" {
		t.Fatalf("pool.Engine() = %q, want %q", got, "postgres")
	}

	// Basic CRUD smoke test: same shape as the MySQL test.
	type user struct {
		ID    string
		Email string
	}

	insertSQL := `INSERT INTO sys_users (id, email, password_hash, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())`
	_, err := pool.Exec(ctx, insertSQL,
		"00000000-0000-0000-0000-000000000001",
		"test@example.com",
		"hashed-password",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var u user
	selectSQL := `SELECT id, email FROM sys_users WHERE id = $1`
	row, qrErr := pool.QueryRow(ctx, selectSQL, "00000000-0000-0000-0000-000000000001")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	if err := row.Scan(&u.ID, &u.Email); err != nil {
		t.Fatalf("select user: %v", err)
	}

	_, err = pool.Exec(ctx, `DELETE FROM sys_users WHERE id = $1`, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}

	t.Log("Postgres 16 container: spin up, migrations, basic CRUD")
}

// TestMSSQLContainer_SpinUp verifies Azure SQL Edge testcontainer startup,
// core migration application, and basic CRUD.
func TestMSSQLContainer_SpinUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL integration test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT is set to a different adapter; skipping MSSQL")
	}

	pool := testdb.MSSQL(t)

	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping mssql: %v", err)
	}

	if got := pool.Engine(); got != "mssql" {
		t.Fatalf("pool.Engine() = %q, want %q", got, "mssql")
	}

	// Basic CRUD smoke test
	insertSQL := `INSERT INTO sys_users (id, email, password_hash, created_at, updated_at)
		VALUES ($1, $2, $3, GETUTCDATE(), GETUTCDATE())`
	result, err := pool.Exec(ctx, insertSQL,
		"00000000-0000-0000-0000-000000000001",
		"test@example.com",
		"hashed-password",
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		t.Fatalf("insert affected %d rows, want 1", n)
	}

	type user struct {
		ID    string
		Email string
	}
	var u user
	selectSQL := `SELECT id, email FROM sys_users WHERE id = $1`
	row, qrErr := pool.QueryRow(ctx, selectSQL, "00000000-0000-0000-0000-000000000001")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	if err := row.Scan(&u.ID, &u.Email); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if u.Email != "test@example.com" {
		t.Errorf("email = %q, want 'test@example.com'", u.Email)
	}

	_, err = pool.Exec(ctx, `UPDATE sys_users SET email = $1 WHERE id = $2`,
		"updated@example.com", "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("update user: %v", err)
	}

	_, err = pool.Exec(ctx, `DELETE FROM sys_users WHERE id = $1`,
		"00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}

	t.Log("MSSQL (Azure SQL Edge): spin up, migrations, CRUD (INSERT/SELECT/UPDATE/DELETE)")
}
