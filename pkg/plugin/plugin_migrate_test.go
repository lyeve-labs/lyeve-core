package plugin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialectDir

func TestDialectDir(t *testing.T) {
	cases := []struct {
		dialect string
		want    string
	}{
		{"postgres", "psql"},
		{"postgresql", "psql"}, // default path
		{"pgx", "psql"},        // default path
		{"mysql", "mysql"},
		{"mssql", "mssql"},
		{"sqlserver", "psql"}, // unrecognized -> default
		{"", "psql"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			assert.Equal(t, tc.want, dialectDir(tc.dialect))
		})
	}
}

// migrationsTableSQL

func TestMigrationsTableSQL(t *testing.T) {
	table := "test_migrations"

	t.Run("postgres", func(t *testing.T) {
		sql := migrationsTableSQL(table, "postgres")
		assert.Contains(t, sql, "CREATE TABLE IF NOT EXISTS")
		assert.Contains(t, sql, "TIMESTAMPTZ")
		assert.Contains(t, sql, "TEXT PRIMARY KEY")
	})

	t.Run("mysql", func(t *testing.T) {
		sql := migrationsTableSQL(table, "mysql")
		assert.Contains(t, sql, "CREATE TABLE IF NOT EXISTS")
		assert.Contains(t, sql, "VARCHAR(255)")
		assert.Contains(t, sql, "ENGINE=InnoDB")
		assert.Contains(t, sql, "DATETIME(6)")
	})

	t.Run("mssql", func(t *testing.T) {
		sql := migrationsTableSQL(table, "mssql")
		assert.Contains(t, sql, "OBJECT_ID")
		assert.Contains(t, sql, "NVARCHAR(255)")
		assert.Contains(t, sql, "DATETIME2(7)")
	})
}

// insertVersionStmt

func TestInsertVersionStmt(t *testing.T) {
	table := "plugin_migrations"

	// Zero affected rows is the signal applyScript uses to decide another boot
	// already claimed a version and the migration can be skipped. Every dialect
	// therefore has to report zero rows for a duplicate and nothing else, which
	// rules out INSERT IGNORE: it reports zero for a deadlock too.
	t.Run("postgres skips only on conflict", func(t *testing.T) {
		sql, args := insertVersionStmt(table, "postgres", "001_initial")
		assert.Contains(t, sql, "ON CONFLICT (version) DO NOTHING")
		assert.Contains(t, sql, "$1")
		assert.Equal(t, []any{"001_initial"}, args)
	})

	t.Run("mysql guards rather than ignores", func(t *testing.T) {
		sql, args := insertVersionStmt(table, "mysql", "001_initial")
		assert.NotContains(t, sql, "INSERT IGNORE",
			"IGNORE reports zero rows for a deadlock, which reads as an already-applied migration")
		assert.Contains(t, sql, "WHERE NOT EXISTS")
		// The version is bound twice: once to insert, once to probe.
		assert.Equal(t, []any{"001_initial", "001_initial"}, args)
	})

	t.Run("mssql merges on no match", func(t *testing.T) {
		sql, args := insertVersionStmt(table, "mssql", "001_initial")
		assert.Contains(t, sql, "MERGE")
		assert.Contains(t, sql, "WHEN NOT MATCHED THEN INSERT")
		assert.Contains(t, sql, "@p1")
		assert.Equal(t, []any{"001_initial"}, args)
	})
}

// loadUpScripts

func TestLoadUpScripts(t *testing.T) {
	t.Run("empty subdirectory", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/.gitkeep": &fstest.MapFile{},
		}
		scripts, err := loadUpScripts(efs, "psql")
		require.NoError(t, err)
		assert.Empty(t, scripts)
	})

	t.Run("only up.sql files loaded", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/001_init.up.sql":    &fstest.MapFile{Data: []byte("CREATE TABLE foo();")},
			"psql/001_init.down.sql":  &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
			"psql/002_indexes.up.sql": &fstest.MapFile{Data: []byte("CREATE INDEX idx_foo ON foo(id);")},
		}
		scripts, err := loadUpScripts(efs, "psql")
		require.NoError(t, err)
		assert.Len(t, scripts, 2)
		assert.Equal(t, "001_init", scripts[0].version)
		assert.Equal(t, "002_indexes", scripts[1].version)
	})

	t.Run("lexicographic ordering", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/010_third.up.sql":  &fstest.MapFile{Data: []byte("SELECT 3;")},
			"psql/001_first.up.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
			"psql/002_second.up.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
		}
		scripts, err := loadUpScripts(efs, "psql")
		require.NoError(t, err)
		require.Len(t, scripts, 3)
		assert.Equal(t, "001_first", scripts[0].version)
		assert.Equal(t, "002_second", scripts[1].version)
		assert.Equal(t, "010_third", scripts[2].version)
	})

	t.Run("skips non-sql files", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/001_init.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t();")},
			"psql/README.md":       &fstest.MapFile{Data: []byte("# migrations")},
			"psql/notes.txt":       &fstest.MapFile{Data: []byte("notes")},
		}
		scripts, err := loadUpScripts(efs, "psql")
		require.NoError(t, err)
		assert.Len(t, scripts, 1)
	})

	t.Run("missing subdirectory", func(t *testing.T) {
		efs := fstest.MapFS{}
		scripts, err := loadUpScripts(efs, "nonexistent")
		require.NoError(t, err, "missing dialect dir returns empty, not error")
		assert.Empty(t, scripts)
	})
}

// loadDownScripts

func TestLoadDownScripts(t *testing.T) {
	t.Run("empty subdirectory", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/.gitkeep": &fstest.MapFile{},
		}
		scripts, err := loadDownScripts(efs, "psql")
		require.NoError(t, err)
		assert.Empty(t, scripts)
	})

	t.Run("only down.sql files loaded", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/001_init.up.sql":      &fstest.MapFile{Data: []byte("CREATE TABLE foo();")},
			"psql/001_init.down.sql":    &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
			"psql/002_indexes.down.sql": &fstest.MapFile{Data: []byte("DROP INDEX idx_foo;")},
		}
		scripts, err := loadDownScripts(efs, "psql")
		require.NoError(t, err)
		assert.Len(t, scripts, 2)
		assert.Contains(t, scripts, "001_init")
		assert.Contains(t, scripts, "002_indexes")
	})

	t.Run("skips non-sql files", func(t *testing.T) {
		efs := fstest.MapFS{
			"psql/001_init.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE t;")},
			"psql/README.md":         &fstest.MapFile{Data: []byte("# migrations")},
			"psql/notes.txt":         &fstest.MapFile{Data: []byte("notes")},
		}
		scripts, err := loadDownScripts(efs, "psql")
		require.NoError(t, err)
		assert.Len(t, scripts, 1)
	})

	t.Run("missing subdirectory", func(t *testing.T) {
		efs := fstest.MapFS{}
		scripts, err := loadDownScripts(efs, "nonexistent")
		require.NoError(t, err, "missing dialect dir returns empty, not error")
		assert.Empty(t, scripts)
	})
}

// PluginMigrate unit tests (nil DB / empty FS)

func TestPluginMigrate_NilDB(t *testing.T) {
	err := PluginMigrate(context.Background(), nil, "postgres", fstest.MapFS{}, "test_migrations")
	assert.NoError(t, err, "nil db should be a no-op")
}

func TestPluginMigrate_EmptyFS(t *testing.T) {
	// With nil db this is trivially a no-op, but if we had a db, empty FS
	// means no scripts to apply. Test that the nil-db case still returns nil
	// even with an empty MapFS.
	err := PluginMigrate(context.Background(), nil, "postgres", fstest.MapFS{}, "test_migrations")
	assert.NoError(t, err)
}

func TestPluginMigrate_NoUpScripts(t *testing.T) {
	// FS that has no .up.sql files in the selected dialect subdir.
	efs := fstest.MapFS{
		"psql/.gitkeep": &fstest.MapFile{},
	}
	// nil db -> no-op, but verifies the path doesn't panic
	err := PluginMigrate(context.Background(), nil, "postgres", efs, "test_migrations")
	assert.NoError(t, err)
}

// isTableNotExistError

func TestIsTableNotExistError(t *testing.T) {
	t.Run("postgres 42P01", func(t *testing.T) {
		err := &pgconn.PgError{Code: "42P01", Message: `relation "nonexistent" does not exist`}
		assert.True(t, IsTableNotExistError(err), "42P01 should be detected as table-not-exist")
	})

	t.Run("postgres other code", func(t *testing.T) {
		err := &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint`}
		assert.False(t, IsTableNotExistError(err), "23505 should NOT be a table-not-exist error")
	})

	t.Run("postgres wrapped in fmt.Errorf", func(t *testing.T) {
		inner := &pgconn.PgError{Code: "42P01"}
		wrapped := fmt.Errorf("query failed: %w", inner)
		assert.True(t, IsTableNotExistError(wrapped), "wrapped 42P01 should still be detected")
	})

	t.Run("postgres wrapped in non-%w fmt", func(t *testing.T) {
		inner := &pgconn.PgError{Code: "42P01"}
		wrapped := fmt.Errorf("query failed: %v", inner) //nolint:errorlint // not %w, intentionally breaks unwrap chain for testing
		assert.False(t, IsTableNotExistError(wrapped), "non-%%w wrapped error should NOT be detected (no unwrap chain)")
	})

	t.Run("mysql 1146", func(t *testing.T) {
		err := &mysql.MySQLError{Number: 1146, Message: "Table 'test.foo' doesn't exist"}
		assert.True(t, IsTableNotExistError(err), "mysql 1146 should be detected")
	})

	t.Run("mysql other number", func(t *testing.T) {
		err := &mysql.MySQLError{Number: 1064, Message: "syntax error"}
		assert.False(t, IsTableNotExistError(err), "mysql 1064 should NOT be a table-not-exist error")
	})

	t.Run("mssql 208", func(t *testing.T) {
		err := mssql.Error{Number: 208, Message: "Invalid object name 'nonexistent'."}
		assert.True(t, IsTableNotExistError(err), "mssql 208 should be detected")
	})

	t.Run("mssql other number", func(t *testing.T) {
		err := mssql.Error{Number: 2627, Message: "Violation of UNIQUE KEY constraint"}
		assert.False(t, IsTableNotExistError(err), "mssql 2627 should NOT be a table-not-exist error")
	})

	t.Run("mssql wrapped in fmt.Errorf", func(t *testing.T) {
		inner := mssql.Error{Number: 208, Message: "Invalid object name"}
		err := fmt.Errorf("outer: %w", inner)
		assert.True(t, IsTableNotExistError(err), "mssql 208 wrapped should be detected")
	})

	t.Run("nil error", func(t *testing.T) {
		assert.False(t, IsTableNotExistError(nil), "nil should not match")
	})

	t.Run("generic error", func(t *testing.T) {
		err := errors.New("random network error")
		assert.False(t, IsTableNotExistError(err), "generic error should not match")
	})

	t.Run("text that only says does not exist", func(t *testing.T) {
		// An error that carries "does not exist" as text but wraps no
		// driver error is not a missing table.
		err := errors.New("file does not exist")
		assert.False(t, IsTableNotExistError(err), "string match alone should not trigger")
	})
}
