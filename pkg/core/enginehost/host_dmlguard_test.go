package enginehost

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// db.IsDML behavioral parity tests

func TestIsDML_RejectsDMLVerbs(t *testing.T) {
	tests := []string{
		"INSERT INTO users (name) VALUES ('x')",
		"  INSERT INTO users (name) VALUES ('x')",
		"\nINSERT INTO users (name) VALUES ('x')",
		"\t\tINSERT INTO users (name) VALUES ('x')",
		"insert into users (name) values ('x')",
		"UPDATE users SET name = 'x' WHERE id = 1",
		"DELETE FROM users WHERE id = 1",
		"TRUNCATE TABLE users",
		"MERGE INTO target USING source ON source.id = target.id WHEN MATCHED THEN UPDATE SET name = source.name WHEN NOT MATCHED THEN INSERT (id, name) VALUES (source.id, source.name)",
		"REPLACE INTO users (id, name) VALUES (1, 'x')",
	}
	for _, sql := range tests {
		t.Run("dml:"+sql[:min(30, len(sql))], func(t *testing.T) {
			assert.True(t, db.IsDML(sql), "expected DML detection for %q", sql)
		})
	}
}

func TestIsDML_AllowsSelectAndDDL(t *testing.T) {
	tests := []string{
		"SELECT * FROM users",
		"SELECT 1",
		"  SELECT count(*) FROM users",
		"CREATE TABLE IF NOT EXISTS migrations (version TEXT PRIMARY KEY)",
		"ALTER TABLE users ADD COLUMN phone TEXT",
		"DROP TABLE IF EXISTS temp",
		"CREATE INDEX idx_name ON users(name)",
		"BEGIN",
		"COMMIT",
		"ROLLBACK",
		"SET search_path = 'tenant_acme'",
		"SAVEPOINT sp_1",
	}
	for _, sql := range tests {
		t.Run("allow:"+sql[:min(30, len(sql))], func(t *testing.T) {
			assert.False(t, db.IsDML(sql), "expected no DML detection for %q", sql)
		})
	}
}

func TestIsDML_CommentsBeforeDML(t *testing.T) {
	tests := []struct {
		sql   string
		isDML bool
	}{
		{"/* purge */ DELETE FROM users WHERE id=1", true},
		{"-- add user\nINSERT INTO users (name) VALUES ('x')", true},
		{"/* list */ SELECT * FROM users", false},
		{"-- list\nSELECT * FROM users", false},
	}
	for _, tt := range tests {
		t.Run(tt.sql[:min(30, len(tt.sql))], func(t *testing.T) {
			assert.Equal(t, tt.isDML, db.IsDML(tt.sql))
		})
	}
}

// engineHost: RawDB returns guarded DB, MigrationDB returns unscoped

func TestEngineHost_RawDB_ReturnsGuardedWhenWired(t *testing.T) {
	raw := openTestDB(t)
	defer raw.Close()

	h := &engineHost{
		rawDB:     raw,
		guardedDB: raw, // same handle for test: real impl uses separate pool
	}

	// With guard wired, RawDB returns the guarded one.
	assert.Same(t, h.guardedDB, h.RawDB())

	// MigrationDB always returns rawDB.
	assert.Same(t, h.rawDB, h.MigrationDB())
}

func TestEngineHost_RawDB_FallsBackWhenNoGuard(t *testing.T) {
	raw := openTestDB(t)
	defer raw.Close()

	h := &engineHost{rawDB: raw, guardedDB: nil}

	// No guard wired: RawDB falls back to rawDB.
	assert.Same(t, h.rawDB, h.RawDB())
	assert.Same(t, h.rawDB, h.MigrationDB())
}

// engineHost: AdminQuerier

func TestEngineHost_AdminQuerier_ReturnsQuerier(t *testing.T) {
	raw := openTestDB(t)
	defer raw.Close()

	h := &engineHost{rawDB: raw}
	q := h.AdminQuerier(context.Background())

	require.NotNil(t, q)

	// SELECT works.
	var val int
	row, qrErr := q.QueryRow(context.Background(), "SELECT 1 AS v")
	require.NoError(t, qrErr, "QueryRow")
	err := row.Scan(&val)
	require.NoError(t, err)
	assert.Equal(t, 1, val)

	// Exec (DDL) works.
	tag, err := q.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS _adminq_test (id INT PRIMARY KEY)")
	require.NoError(t, err)
	t.Cleanup(func() { q.Exec(context.Background(), "DROP TABLE IF EXISTS _adminq_test") })
	_ = tag

	// DML works through AdminQuerier (that's the point).
	tag, err = q.Exec(context.Background(), "INSERT INTO _adminq_test (id) VALUES (42)")
	require.NoError(t, err)
	n := tag.RowsAffected
	assert.Greater(t, n, int64(0))

	var id int
	row, qrErr = q.QueryRow(context.Background(), "SELECT id FROM _adminq_test WHERE id = 42")
	require.NoError(t, qrErr, "QueryRow")
	err = row.Scan(&id)
	require.NoError(t, err)
	assert.Equal(t, 42, id)
}

func TestEngineHost_AdminQuerier_BeginCommit(t *testing.T) {
	raw := openTestDB(t)
	defer raw.Close()

	h := &engineHost{rawDB: raw}
	q := h.AdminQuerier(context.Background())

	// Setup.
	q.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS _adminq_tx (id INT PRIMARY KEY, val TEXT)")
	t.Cleanup(func() { q.Exec(context.Background(), "DROP TABLE IF EXISTS _adminq_tx") })

	tx, err := q.Begin(context.Background())
	require.NoError(t, err)

	_, err = tx.Exec(context.Background(), "INSERT INTO _adminq_tx (id, val) VALUES (1, 'a')")
	require.NoError(t, err)

	err = tx.Commit(context.Background())
	require.NoError(t, err)

	var v string
	row, qrErr := q.QueryRow(context.Background(), "SELECT val FROM _adminq_tx WHERE id = 1")
	require.NoError(t, qrErr, "QueryRow")
	err = row.Scan(&v)
	require.NoError(t, err)
	assert.Equal(t, "a", v)
}

func TestEngineHost_AdminQuerier_Rollback(t *testing.T) {
	raw := openTestDB(t)
	defer raw.Close()

	h := &engineHost{rawDB: raw}
	q := h.AdminQuerier(context.Background())

	q.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS _adminq_tx (id INT PRIMARY KEY, val TEXT)")
	t.Cleanup(func() { q.Exec(context.Background(), "DROP TABLE IF EXISTS _adminq_tx") })

	tx, err := q.Begin(context.Background())
	require.NoError(t, err)

	_, err = tx.Exec(context.Background(), "INSERT INTO _adminq_tx (id, val) VALUES (2, 'rollback')")
	require.NoError(t, err)

	err = tx.Rollback(context.Background())
	require.NoError(t, err)

	var count int
	row, qrErr := q.QueryRow(context.Background(), "SELECT COUNT(*) FROM _adminq_tx WHERE id = 2")
	require.NoError(t, qrErr, "QueryRow")
	err = row.Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "rolled back row should be absent")
}

// engineHost: DMLGuard on RawDB integration test

func TestEngineHost_RawDB_RejectsDMLWhenGuarded(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires running PG")
	}
	// Open a real DB for the guarded pool.
	raw := openTestDB(t)
	defer raw.Close()

	// Open a separate DML-guarded DB against the same database.
	guard, err := db.NewDMLGuardDB(context.Background(), "postgres", "postgres://lyeve:***@localhost:5432/lyeve?sslmode=disable")
	if err != nil {
		t.Skipf("no test PG available: %v", err)
	}
	defer guard.Close()

	h := &engineHost{
		rawDB:     raw,
		guardedDB: guard,
	}

	// Setup: create table through the unscoped AdminQuerier.
	q := h.AdminQuerier(context.Background())
	q.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS _dml_test (id INT PRIMARY KEY, val TEXT)")
	t.Cleanup(func() { q.Exec(context.Background(), "DROP TABLE IF EXISTS _dml_test") })

	// RawDB returns the guarded DB.
	guardedResult := h.RawDB()

	// SELECT through RawDB works.
	var one int
	err = guardedResult.QueryRowContext(context.Background(), "SELECT 1 AS v").Scan(&one)
	require.NoError(t, err)
	assert.Equal(t, 1, one)

	// DML through RawDB is rejected.
	_, err = guardedResult.ExecContext(context.Background(), "INSERT INTO _dml_test (id, val) VALUES (1, 'x')")
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrDMLRejected),
		"expected ErrDMLRejected, got %v", err)

	// DDL through RawDB still works (for PluginMigrate).
	_, err = guardedResult.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS _dml_mig (version TEXT PRIMARY KEY)")
	require.NoError(t, err)
	guardedResult.ExecContext(context.Background(), "DROP TABLE IF EXISTS _dml_mig")

	// Verify the DML was actually rejected (row not inserted).
	var count int
	row, qrErr := q.QueryRow(context.Background(), "SELECT COUNT(*) FROM _dml_test")
	require.NoError(t, qrErr, "QueryRow")
	err = row.Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// MigrationDB is still the real, unscoped pool.
	migDB := h.MigrationDB()
	assert.Same(t, h.rawDB, migDB)

	// DML through MigrationDB works (it's the real pool).
	_, err = migDB.ExecContext(context.Background(), "INSERT INTO _dml_test (id, val) VALUES (99, 'via-migration')")
	require.NoError(t, err, "MigrationDB should allow DML")
}

// helpers

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://lyeve:***@localhost:5432/lyeve?sslmode=disable")
	if err != nil {
		t.Skipf("no test PG: %v", err)
	}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("PG ping failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
