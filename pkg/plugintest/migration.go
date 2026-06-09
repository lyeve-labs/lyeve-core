package plugintest

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// MigratePlugin runs core.PluginMigrate against the test host's database
// via host.MigrationDB().
func MigratePlugin(ctx context.Context, host core.Host, migrationsFS fs.FS, tableName string) error {
	return core.PluginMigrate(ctx, host.MigrationDB(), host.Dialect(), migrationsFS, tableName)
}

// AssertMigrationTable fails the test if the migration tracking table does
// not exist. Use this to verify that MigratePlugin ran successfully.
//
//	plugintest.AssertMigrationTable(t, ctx, host, "plugin_my_plugin_schema_migrations")
func AssertMigrationTable(t T, ctx context.Context, host core.Host, tableName string) {
	t.Helper()
	var count int
	row, err := host.Querier(ctx).QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1`,
		tableName)
	if err != nil {
		t.Fatalf("AssertMigrationTable: query information_schema: %v", err)
	}
	if err := row.Scan(&count); err != nil {
		t.Fatalf("AssertMigrationTable: query information_schema: %v", err)
	}
	if count == 0 {
		t.Errorf("expected migration table %q to exist, but it does not", tableName)
	}
}

// AssertMigrationCount fails if the migration tracking table doesn't have
// exactly want entries.
func AssertMigrationCount(t T, ctx context.Context, host core.Host, tableName string, want int) {
	t.Helper()
	var count int
	row, err := host.Querier(ctx).QueryRow(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName))
	if err != nil {
		t.Fatalf("AssertMigrationCount: query %s: %v", tableName, err)
	}
	if err := row.Scan(&count); err != nil {
		t.Fatalf("AssertMigrationCount: query %s: %v", tableName, err)
	}
	if count != want {
		t.Errorf("expected %d migration entries in %q, got %d", want, tableName, count)
	}
}

// PrepareMigrationHost is a shortcut: it wraps a testcontainers DB pool
// and raw *sql.DB into a plugintest Host. Use this when you want the
// full host (with hooks, config) without starting from the Postgres/MySQL/
// MSSQL convenience wrappers. The returned Host has Dialect() set and
// Querier/RawDB wired, but Hooks returns a HookSpy and Schema returns nil.
//
// The pool is any value with the dbPool methods (Engine, SQLDB, QueryRow,
// Query, Exec and Begin), such as a small wrapper around a *sql.DB:
//
//	host := plugintest.PrepareMigrationHost(t, pool)
func PrepareMigrationHost(t T, pool dbPool) core.Host {
	t.Helper()
	return &testHost{
		pool:    pool,
		rawDB:   pool.SQLDB(),
		dialect: pool.Engine(),
	}
}
