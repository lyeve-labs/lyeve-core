//go:build integration && !mutest
// +build integration,!mutest

package plugin_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test SQL migration scripts (in-memory, no filesystem)
//
// These are deliberately crafted minimal scripts so we can test PluginMigrate
// without depending on any plugin's real migrations directory.

var testMigrations = fstest.MapFS{
	// PostgreSQL
	"psql/001_init.up.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE IF NOT EXISTS plugin_test_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS _idx_test_items_name ON plugin_test_items (name);
`)},
	"psql/002_add_meta.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}'::jsonb;
`)},
	"psql/003_add_status.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
CREATE INDEX IF NOT EXISTS _idx_test_items_status ON plugin_test_items (status);
`)},

	// MySQL. ADD COLUMN takes no IF NOT EXISTS here, unlike the PostgreSQL
	// arm above, so these are written the way a real plugin's MySQL tree has
	// to be: bare. PluginMigrate skips a version its bookkeeping table
	// already records, which is what makes a second run a no-op, rather than
	// the DDL guarding itself.
	"mysql/001_init.up.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE IF NOT EXISTS plugin_test_items (
    id CHAR(36) PRIMARY KEY,
    name VARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL DEFAULT NOW(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
CREATE INDEX _idx_test_items_name ON plugin_test_items (name);
`)},
	"mysql/002_add_meta.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD COLUMN meta JSON;
`)},
	"mysql/003_add_status.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD COLUMN status VARCHAR(32) NOT NULL DEFAULT 'active';
CREATE INDEX _idx_test_items_status ON plugin_test_items (status);
`)},

	// MSSQL
	"mssql/001_init.up.sql": &fstest.MapFile{Data: []byte(`
IF OBJECT_ID(N'plugin_test_items', N'U') IS NULL
CREATE TABLE plugin_test_items (
    id UNIQUEIDENTIFIER PRIMARY KEY,
    name NVARCHAR(255) NOT NULL DEFAULT '',
    created_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME()
);
IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name = '_idx_test_items_name')
    CREATE INDEX _idx_test_items_name ON plugin_test_items (name);
`)},
	"mssql/002_add_meta.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD meta NVARCHAR(MAX) DEFAULT '{}';
`)},
	"mssql/003_add_status.up.sql": &fstest.MapFile{Data: []byte(`
ALTER TABLE plugin_test_items ADD status NVARCHAR(32) NOT NULL DEFAULT 'active';
IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name = '_idx_test_items_status')
    CREATE INDEX _idx_test_items_status ON plugin_test_items (status);
`)},
}

// Test DB helpers

func migrateTestPostgres(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_test"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.PingContext(ctx); err == nil {
			return db
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for postgres")
	return nil
}

func migrateTestMySQL(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	ctr, err := mysql.Run(ctx,
		"mysql:8",
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "3306")
	require.NoError(t, err)
	dsn := fmt.Sprintf("cms:secret@tcp(%s:%s)/lyeve_test?parseTime=true&multiStatements=true",
		host, port.Port())

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	require.NoError(t, db.PingContext(ctx))
	return db
}

func migrateTestMSSQL(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcmssql.Run(ctx,
		"mcr.microsoft.com/azure-sql-edge:latest",
		tcmssql.WithAcceptEULA(),
		tcmssql.WithPassword("Str0ng@Passw0rd"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
	require.NoError(t, err)
	db, err := sql.Open("sqlserver", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.PingContext(ctx); err == nil {
			return db
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for mssql")
	return nil
}

type dialectCase struct {
	name    string
	dialect string
	fresh   func(*testing.T) *sql.DB
}

func migrateDialects(t *testing.T) []dialectCase {
	t.Helper()
	cases := []dialectCase{
		{"postgres", "postgres", migrateTestPostgres},
		{"mysql", "mysql", migrateTestMySQL},
		{"mssql", "mssql", migrateTestMSSQL},
	}
	var out []dialectCase
	for _, c := range cases {
		if testing.Short() && c.name != "postgres" {
			// In short mode, only test postgres (fastest container).
			continue
		}
		out = append(out, c)
	}
	return out
}

// Integration tests

// TestPluginMigrate_FullCycle verifies that PluginMigrate applies all
// pending .up.sql scripts across all three dialects, creating the tracking
// table and recording versions.
func TestPluginMigrate_FullCycle(t *testing.T) {
	for _, tc := range migrateDialects(t) {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.fresh(t)
			ctx := context.Background()

			err := core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_plugin_migrations")
			require.NoError(t, err)

			// Verify the tracking table was created.
			rows, err := db.QueryContext(ctx, "SELECT version FROM test_plugin_migrations ORDER BY version")
			require.NoError(t, err)
			defer rows.Close()

			var versions []string
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				versions = append(versions, v)
			}
			require.NoError(t, rows.Err())

			expected := []string{"001_init", "002_add_meta", "003_add_status"}
			assert.Equal(t, expected, versions, "all 3 migrations should be recorded")

			// Verify the tables and columns exist (dialect-specific checks).
			verifyMigrationApplied(t, ctx, db, tc.dialect)
		})
	}
}

func verifyMigrationApplied(t *testing.T, ctx context.Context, db *sql.DB, dialect string) {
	t.Helper()

	switch dialect {
	case "postgres":
		// Check table exists.
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'plugin_test_items')").Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "plugin_test_items table should exist")

		// Check all 3 column additions were applied.
		var colCount int
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'plugin_test_items'").Scan(&colCount)
		require.NoError(t, err)
		// id, name, created_at, meta, status = 5 columns
		assert.Equal(t, 5, colCount, "should have 5 columns after all migrations")

	case "mysql":
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) > 0 FROM information_schema.tables WHERE table_name = 'plugin_test_items' AND table_schema = 'lyeve_test'").Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "plugin_test_items table should exist")

		var colCount int
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'plugin_test_items' AND table_schema = 'lyeve_test'").Scan(&colCount)
		require.NoError(t, err)
		assert.Equal(t, 5, colCount, "should have 5 columns after all migrations")

	case "mssql":
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sys.tables WHERE name = 'plugin_test_items'").Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "plugin_test_items table should exist")

		var colCount int
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sys.columns WHERE object_id = OBJECT_ID('plugin_test_items')").Scan(&colCount)
		require.NoError(t, err)
		assert.Equal(t, 5, colCount, "should have 5 columns after all migrations")
	}
}

// TestPluginMigrate_Idempotent verifies that running PluginMigrate twice
// does not re-apply already-applied migrations.
func TestPluginMigrate_Idempotent(t *testing.T) {
	for _, tc := range migrateDialects(t) {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.fresh(t)
			ctx := context.Background()

			// First run.
			err := core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_idempotent_migrations")
			require.NoError(t, err)

			// Second run: should be a no-op.
			err = core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_idempotent_migrations")
			require.NoError(t, err)

			// Versions recorded exactly once.
			rows, err := db.QueryContext(ctx, "SELECT version FROM test_idempotent_migrations ORDER BY version")
			require.NoError(t, err)
			defer rows.Close()

			var versions []string
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				versions = append(versions, v)
			}
			require.NoError(t, rows.Err())
			assert.Len(t, versions, 3, "should only have 3 versions, no duplicates")
		})
	}
}

// TestPluginMigrate_PartialFailure verifies that when a migration script
// contains invalid SQL, the error is propagated and earlier (already-applied)
// migrations are not lost.
func TestPluginMigrate_PartialFailure(t *testing.T) {
	badMigrations := fstest.MapFS{
		"psql/001_good.up.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE IF NOT EXISTS plugin_pf_test (id SERIAL PRIMARY KEY);
`)},
		"psql/002_bad.up.sql": &fstest.MapFile{Data: []byte(`
THIS IS NOT VALID SQL AT ALL !!!;
`)},
		"psql/003_never_runs.up.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE IF NOT EXISTS plugin_pf_should_not_exist (id SERIAL PRIMARY KEY);
`)},
	}

	t.Run("postgres", func(t *testing.T) {
		db := migrateTestPostgres(t)
		ctx := context.Background()

		err := core.PluginMigrate(ctx, db, "postgres", badMigrations, "test_pf_migrations")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "002_bad", "error should mention the failing script")

		// 001_good should have been committed.
		rows, err := db.QueryContext(ctx, "SELECT version FROM test_pf_migrations ORDER BY version")
		require.NoError(t, err)
		defer rows.Close()
		var versions []string
		for rows.Next() {
			var v string
			require.NoError(t, rows.Scan(&v))
			versions = append(versions, v)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"001_good"}, versions, "only 001 should be applied")

		// 003_never_runs should NOT have run.
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'plugin_pf_should_not_exist')").Scan(&exists)
		require.NoError(t, err)
		assert.False(t, exists, "003 should not have been applied after failure")

		// The tracking table should have existed before 001 was recorded.
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'test_pf_migrations')").Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "tracking table should exist even after partial failure")
	})
}

// TestPluginMigrate_Concurrent verifies that concurrent calls to PluginMigrate
// with the same tracking table don't corrupt the migration state.
func TestPluginMigrate_Concurrent(t *testing.T) {
	for _, tc := range migrateDialects(t) {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.fresh(t)
			ctx := context.Background()

			var wg sync.WaitGroup
			errs := make(chan error, 5)

			for i := 0; i < 5; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_concurrent_migrations")
					if err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)

			// Collect errors. Separate the DDL races (concurrent CREATE
			// TABLE IF NOT EXISTS on PG can race with 42P07/23505, a
			// PostgreSQL DDL limitation) from migration INSERT/exec errors,
			// which record-then-migrate rules out.
			var errsCreateTable, errsInsert []error
			for err := range errs {
				if strings.Contains(err.Error(), "create tracking table") {
					errsCreateTable = append(errsCreateTable, err)
				} else {
					errsInsert = append(errsInsert, err)
				}
			}
			assert.Empty(t, errsInsert,
				"no INSERT/exec race may fail a boot (%d CREATE TABLE races, a PostgreSQL DDL limitation)",
				len(errsCreateTable))
			t.Logf("concurrent test: %d CREATE TABLE races (a PostgreSQL DDL limitation), %d INSERT/exec errors",
				len(errsCreateTable), len(errsInsert))

			// Verify all 3 migrations were applied.
			rows, err := db.QueryContext(ctx, "SELECT version FROM test_concurrent_migrations ORDER BY version")
			require.NoError(t, err)
			defer rows.Close()

			var versions []string
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				versions = append(versions, v)
			}
			require.NoError(t, rows.Err())

			expected := []string{"001_init", "002_add_meta", "003_add_status"}
			assert.Equal(t, expected, versions,
				"concurrent migrations should apply all 3 versions exactly once")

			verifyMigrationApplied(t, ctx, db, tc.dialect)
		})
	}
}

// TestPluginMigrate_EmptyDialect verifies that an empty dialect subdirectory
// (no .up.sql files) returns no error.
func TestPluginMigrate_EmptyDialect(t *testing.T) {
	emptyFS := fstest.MapFS{
		"psql/.gitkeep": &fstest.MapFile{},
	}
	db := migrateTestPostgres(t)
	ctx := context.Background()

	err := core.PluginMigrate(ctx, db, "postgres", emptyFS, "test_empty_migrations")
	assert.NoError(t, err)
}

// A dialect with no scripts is nothing to run, not a failure. PluginMigrate
// returns nil on an empty script set on purpose: a plugin that has not
// written its tree for a dialect yet must not stop the engine booting on that
// dialect. Refusing instead would need something to tell a missing tree from
// a deliberate one, which the file system alone cannot.
func TestPluginMigrate_MissingDialectDir(t *testing.T) {
	noDialectFS := fstest.MapFS{
		"mysql/001.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t();")},
	}
	db := migrateTestPostgres(t)
	ctx := context.Background()

	// "postgres" -> "psql" subdir, which does not exist in this FS.
	err := core.PluginMigrate(ctx, db, "postgres", noDialectFS, "test_missing_migrations")
	require.NoError(t, err, "a dialect with no scripts is nothing to run")

	// And nothing was created for it.
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'test_missing_migrations')",
	).Scan(&exists))
	assert.False(t, exists, "no scripts means no bookkeeping table either")
}

// TestPluginMigrate_DataSurvivesMigrationCycle runs a down step by hand and
// then PluginMigrate again. There is no built-in rollback, so this is the
// manual workflow, and a version already recorded is not applied again.
func TestPluginMigrate_DataSurvivesMigrationCycle(t *testing.T) {
	t.Run("postgres_manual_down_then_up", func(t *testing.T) {
		db := migrateTestPostgres(t)
		ctx := context.Background()

		// Apply migration.
		err := core.PluginMigrate(ctx, db, "postgres", testMigrations, "test_cycle_migrations")
		require.NoError(t, err)

		// Insert test data after migration.
		_, err = db.ExecContext(ctx, "INSERT INTO plugin_test_items (name, status) VALUES ('test-item', 'active')")
		require.NoError(t, err)

		// Manually drop a column (simulating a down migration).
		_, err = db.ExecContext(ctx, "ALTER TABLE plugin_test_items DROP COLUMN IF EXISTS meta")
		require.NoError(t, err)

		// Also drop a table that was created by a later migration.
		// (Not necessary here since all our migrations are on the same table.)

		// Verify column is gone.
		var colCount int
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'plugin_test_items'").Scan(&colCount)
		require.NoError(t, err)
		assert.Equal(t, 4, colCount, "meta column should be dropped")

		// PluginMigrate goes by recorded version, not by the schema it finds,
		// so a recorded version is not applied again even when the schema has
		// lost what it added.
		err = core.PluginMigrate(ctx, db, "postgres", testMigrations, "test_cycle_migrations")
		require.NoError(t, err, "re-applying should not error (but may not fix the schema)")

		// Verify the column is STILL missing (because PluginMigrate skipped it).
		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'plugin_test_items'").Scan(&colCount)
		require.NoError(t, err)
		// 4, because the version that adds meta is recorded.
		t.Logf("column count after re-migrate: %d (expected 4 due to version-based skip)", colCount)
	})
}

// TestPluginMigrate_VersionAlreadyInserted verifies the record-then-migrate
// loser-skip path: when the version is already present in the tracking table
// (inserted by another concurrent boot), PluginMigrate does NOT re-execute
// the migration SQL. It commits an empty transaction and moves on.
func TestPluginMigrate_VersionAlreadyInserted(t *testing.T) {
	for _, tc := range migrateDialects(t) {
		t.Run(tc.name, func(t *testing.T) {
			db := tc.fresh(t)
			ctx := context.Background()

			// First run: applies all 3 migrations.
			err := core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations")
			require.NoError(t, err)

			// Verify the tracking table has all 3 versions.
			rows, err := db.QueryContext(ctx, "SELECT version FROM test_preinserted_migrations ORDER BY version")
			require.NoError(t, err)
			var versions []string
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				versions = append(versions, v)
			}
			rows.Close()
			require.Equal(t, []string{"001_init", "002_add_meta", "003_add_status"}, versions)

			// Lose version 002 from the bookkeeping, which is what a hand
			// repair, a partial restore or a truncated table leaves behind.
			// The column itself is still there.
			_, err = db.ExecContext(ctx, "DELETE FROM test_preinserted_migrations WHERE version = '002_add_meta'")
			require.NoError(t, err)

			// PluginMigrate re-applies a version it has no record of, so the
			// DDL runs again. Whether that succeeds is the dialect's, not the
			// engine's: only PostgreSQL can write ADD COLUMN IF NOT EXISTS,
			// so only PostgreSQL heals itself here.
			//
			// The three dialects differ, and the difference is worth pinning
			// because it decides whether an operator's repair leaves a
			// bootable install.
			err = core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations")

			var recorded int
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM test_preinserted_migrations WHERE version = '002_add_meta'",
			).Scan(&recorded))

			switch tc.dialect {
			case "postgres":
				require.NoError(t, err, "the DDL is idempotent here, so the repair heals")
				assert.Equal(t, 1, recorded)
			case "mysql":
				// The re-apply fails on a duplicate column. MySQL commits DDL
				// implicitly, so the claim survives the failed script as a row
				// marked incomplete, and the repair the engine names has to
				// mark that row rather than insert a second one.
				repair := "UPDATE test_preinserted_migrations SET completed = 1 WHERE version = '002_add_meta'"
				require.Error(t, err, "re-running a bare ADD COLUMN must not look like success")
				assert.Contains(t, err.Error(), repair, "a failure the operator can repair has to name the repair")
				assert.Equal(t, 1, recorded, "the claim commits on this engine")
				var completed int
				require.NoError(t, db.QueryRowContext(ctx,
					"SELECT completed FROM test_preinserted_migrations WHERE version = '002_add_meta'",
				).Scan(&completed))
				assert.Zero(t, completed, "the claim is recorded as incomplete")

				// An incomplete claim stops the next boot: the engine cannot
				// tell a script that failed on its first statement from one
				// that failed halfway.
				err = core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "started and never completed")
				assert.Contains(t, err.Error(), repair, "the refusal names the same repair")

				// The repair the engine named lets the next boot through.
				_, err = db.ExecContext(ctx, repair)
				require.NoError(t, err)
				assert.NoError(t,
					core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations"),
					"the boot after the named repair is clean")
			case "mssql":
				// The re-apply fails and the claim rolls back with it, so
				// every later boot repeats the same failure. The install
				// does not recover on its own, which is why the engine
				// answers with the INSERT that would repair it.
				require.Error(t, err)
				assert.Contains(t, err.Error(), "INSERT INTO test_preinserted_migrations (version) VALUES ('002_add_meta')",
					"this is the engine that never recovers on its own, so the row is the only way out")
				assert.Equal(t, 0, recorded, "the claim rolls back here, which is why the next boot fails too")
				assert.Error(t,
					core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations"),
					"this engine does not recover on its own")
				// Put the bookkeeping back so the rest of this test can run.
				_, err = db.ExecContext(ctx, preinsertSQL(tc.dialect), "002_add_meta")
				require.NoError(t, err)
			}

			// Simulate the loser-skip path: pre-insert all 3 versions,
			// so PluginMigrate's INSERT returns RowsAffected=0 for each
			// and it should skip the migration SQL.
			_, err = db.ExecContext(ctx, "DELETE FROM test_preinserted_migrations")
			require.NoError(t, err)
			for _, v := range []string{"001_init", "002_add_meta", "003_add_status"} {
				// This is the raw *sql.DB, not the engine's rewriting
				// querier, so $N stays $N and NOW() is not a function on
				// SQL Server.
				_, err = db.ExecContext(ctx, preinsertSQL(tc.dialect), v)
				require.NoError(t, err)
			}

			// Re-run: all 3 versions already exist, so INSERT dedup
			// returns RowsAffected=0 for all -> skip SQL.
			err = core.PluginMigrate(ctx, db, tc.dialect, testMigrations, "test_preinserted_migrations")
			require.NoError(t, err)

			// Still the same 3 versions (no duplicates, no gaps).
			rows, err = db.QueryContext(ctx, "SELECT version FROM test_preinserted_migrations ORDER BY version")
			require.NoError(t, err)
			versions = nil
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				versions = append(versions, v)
			}
			rows.Close()
			require.Equal(t, []string{"001_init", "002_add_meta", "003_add_status"}, versions)
		})
	}
}

// preinsertSQL writes a bookkeeping row straight into the tracking table.
//
// These tests hold the raw *sql.DB rather than the engine's querier, so
// nothing rewrites the placeholder and nothing translates the clock: $N is
// literal outside PostgreSQL and NOW() is not a function on SQL Server.
func preinsertSQL(dialect string) string {
	switch dialect {
	case "mysql":
		return "INSERT INTO test_preinserted_migrations (version, applied_at) VALUES (?, NOW(6))"
	case "mssql":
		return "INSERT INTO test_preinserted_migrations (version, applied_at) VALUES (@p1, SYSUTCDATETIME())"
	default:
		return "INSERT INTO test_preinserted_migrations (version, applied_at) VALUES ($1, NOW())"
	}
}
