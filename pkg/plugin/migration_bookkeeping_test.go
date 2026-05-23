package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

type bookkeepingDialect struct {
	name string
	open func(t *testing.T) *sql.DB
}

func bookkeepingDialects() []bookkeepingDialect {
	all := []bookkeepingDialect{
		{"postgres", func(t *testing.T) *sql.DB { return testdb.Postgres(t).SQLDB() }},
		{"mysql", func(t *testing.T) *sql.DB { return testdb.MySQL(t).SQLDB() }},
		{"mssql", func(t *testing.T) *sql.DB { return testdb.MSSQL(t).SQLDB() }},
	}
	var out []bookkeepingDialect
	for _, d := range all {
		if testdb.ShouldTest(d.name) {
			out = append(out, d)
		}
	}
	return out
}

// createTableSQL is DDL every dialect accepts. On MySQL it also commits the
// transaction it runs in.
func createTableSQL(_, table string) string {
	return fmt.Sprintf("CREATE TABLE %s (id INT NOT NULL PRIMARY KEY)", table)
}

func scriptFS(dialect string, scripts map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range scripts {
		fsys[dialectDir(dialect)+"/"+name+".up.sql"] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestPluginMigrate_ScriptThatFailedPartwayStopsTheNextBoot(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			fsys := scriptFS(d.name, map[string]string{
				"001_multi": createTableSQL(d.name, "bk_partial_first") + ";\nTHIS IS NOT SQL;",
			})

			err := PluginMigrate(ctx, db, d.name, fsys, "bk_partial_migrations")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "001_multi")

			err = PluginMigrate(ctx, db, d.name, fsys, "bk_partial_migrations")
			require.Error(t, err, "a script that never finished must not read as applied")
			if d.name == "mysql" {
				assert.Contains(t, err.Error(), "never completed")
				assert.Contains(t, err.Error(), "bk_partial_migrations")
			}
		})
	}
}

func TestPluginMigrate_RecordsCompletionAndChecksum(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			script := createTableSQL(d.name, "bk_done_first")
			fsys := scriptFS(d.name, map[string]string{"001_init": script})

			require.NoError(t, PluginMigrate(ctx, db, d.name, fsys, "bk_done_migrations"))
			require.NoError(t, PluginMigrate(ctx, db, d.name, fsys, "bk_done_migrations"))

			recorded, err := recordedVersions(ctx, db, "bk_done_migrations")
			require.NoError(t, err)
			assert.Equal(t, map[string]recordedVersion{
				"001_init": {completed: true, checksum: scriptChecksum(script)},
			}, recorded)
		})
	}
}

func TestPluginMigrate_EditedScriptStopsTheBoot(t *testing.T) {
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()

			require.NoError(t, PluginMigrate(ctx, db, d.name,
				scriptFS(d.name, map[string]string{"001_init": createTableSQL(d.name, "bk_edit_old")}),
				"bk_edit_migrations"))

			err := PluginMigrate(ctx, db, d.name,
				scriptFS(d.name, map[string]string{"001_init": createTableSQL(d.name, "bk_edit_new")}),
				"bk_edit_migrations")
			require.Error(t, err, "an applied script whose contents changed must not be skipped by name")
			assert.Contains(t, err.Error(), "001_init")
			assert.Contains(t, err.Error(), "different contents")
		})
	}
}

func TestPluginMigrate_RowsWithoutCompletionCountAsApplied(t *testing.T) {
	bare := map[string]string{
		"postgres": "CREATE TABLE bk_bare_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
		"mysql":    "CREATE TABLE bk_bare_migrations (version VARCHAR(255) NOT NULL PRIMARY KEY, applied_at DATETIME(6) NOT NULL DEFAULT NOW(6))",
		"mssql":    "CREATE TABLE bk_bare_migrations (version NVARCHAR(255) NOT NULL PRIMARY KEY, applied_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME())",
	}
	for _, d := range bookkeepingDialects() {
		t.Run(d.name, func(t *testing.T) {
			db := d.open(t)
			ctx := context.Background()
			_, err := db.ExecContext(ctx, bare[d.name])
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "INSERT INTO bk_bare_migrations (version) VALUES ('001_first')")
			require.NoError(t, err)

			// 001_first would fail if it ran, so success proves it was skipped.
			fsys := scriptFS(d.name, map[string]string{
				"001_first":  "THIS IS NOT SQL;",
				"002_second": createTableSQL(d.name, "bk_bare_next"),
			})
			require.NoError(t, PluginMigrate(ctx, db, d.name, fsys, "bk_bare_migrations"))
			require.NoError(t, PluginMigrate(ctx, db, d.name, fsys, "bk_bare_migrations"))

			recorded, err := recordedVersions(ctx, db, "bk_bare_migrations")
			require.NoError(t, err)
			assert.Equal(t, recordedVersion{completed: true}, recorded["001_first"])
			assert.True(t, recorded["002_second"].completed)
			assert.NotEmpty(t, recorded["002_second"].checksum)
		})
	}
}
