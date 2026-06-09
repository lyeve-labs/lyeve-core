package plugintest

import (
	"context"
	"io/fs"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// supportedDialects is the ordered list of dialects that ForEachDialect
// iterates. PostgreSQL runs first to surface PG-specific issues early,
// then MySQL, then MSSQL.
var supportedDialects = []string{"postgres", "mysql", "mssql"}

// dialectHostFactory maps dialect name -> host factory function.
// Each factory spins up a shared testcontainer (once per process), creates
// a fresh database, runs core migrations, and returns a core.Host.
var dialectHostFactory = map[string]func(t *testing.T) core.Host{
	"postgres": Postgres,
	"mysql":    MySQL,
	"mssql":    MSSQL,
}

// ForEachDialect runs fn once for each enabled database dialect (postgres,
// mysql, mssql). Dialects disabled by CI_DIALECT are skipped. PostgreSQL
// runs first, then MySQL, then MSSQL. Each dialect subtest gets its own
// *testing.T scoped to that dialect, so t.Skip/t.Fatalf apply per-dialect.
//
// The host is backed by a fresh database within a shared container and is
// cleaned up automatically when the subtest returns. Its pool admits
// testdb.DefaultMaxConns simultaneous sessions, which bounds any concurrency
// the test issues: for a test whose subject is contention, use
// ForEachDialectConcurrent and size the pool to the burst.
//
//	func TestCRUD_AllDialects(t *testing.T) {
//	    plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
//	        p := plugin.New()
//	        require.NoError(t, p.Start(context.Background(), host))
//	        defer p.Stop(context.Background())
//	        // ...test CRUD...
//	    })
//	}
func ForEachDialect(t *testing.T, fn func(t *testing.T, host core.Host)) {
	t.Helper()
	for _, dialect := range supportedDialects {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			factory, ok := dialectHostFactory[dialect]
			if !ok {
				t.Fatalf("no host factory for dialect %q", dialect)
			}
			host := factory(t)
			fn(t, host)
		})
	}
}

var dialectHostOptionFactory = map[string]func(t *testing.T, opts ...HostOption) core.Host{
	"postgres": PostgresWithOptions,
	"mysql":    MySQLWithOptions,
	"mssql":    MSSQLWithOptions,
}

// ForEachDialectWithOptions is ForEachDialect with host options applied to
// every dialect's host.
//
// Behavior that branches on configuration has to be proven on each engine like
// any other, and without this a test that needs an option could only reach the
// dialect whose WithOptions constructor it named.
func ForEachDialectWithOptions(t *testing.T, opts []HostOption, fn func(t *testing.T, host core.Host)) {
	t.Helper()
	for _, dialect := range supportedDialects {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			factory, ok := dialectHostOptionFactory[dialect]
			if !ok {
				t.Fatalf("no host factory for dialect %q", dialect)
			}
			fn(t, factory(t, opts...))
		})
	}
}

// ForEachDialectConcurrent is ForEachDialect with the pool sized by the caller.
//
// The hosts ForEachDialect builds cap the pool at testdb.DefaultMaxConns, which
// is small so that many suites can share one container per dialect. That cap
// also bounds how many callers a test can get into the database at once, so a
// burst test written against the default reproduces far less concurrency than
// it appears to. Use this when the behavior under test is the concurrency
// itself, and size maxConns to the burst.
func ForEachDialectConcurrent(t *testing.T, maxConns int, fn func(t *testing.T, host core.Host)) {
	t.Helper()
	factories := map[string]func(*testing.T, int, ...HostOption) core.Host{
		"postgres": ConcurrentPostgres,
		"mysql":    ConcurrentMySQL,
		"mssql":    ConcurrentMSSQL,
	}
	for _, dialect := range supportedDialects {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			factory, ok := factories[dialect]
			if !ok {
				t.Fatalf("no host factory for dialect %q", dialect)
			}
			fn(t, factory(t, maxConns))
		})
	}
}

// ForEachDialectParallel is like ForEachDialect but marks each dialect subtest
// as parallel (t.Parallel). All enabled dialects run concurrently.
//
// WARNING: parallel dialect tests share the same testcontainers (one per
// dialect, started once per process). The per-test databases are unique and
// isolated, but container-level resource contention (CPU, memory) can cause
// slower results than sequential execution in constrained environments.
func ForEachDialectParallel(t *testing.T, fn func(t *testing.T, host core.Host)) {
	t.Helper()
	for _, dialect := range supportedDialects {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		t.Run(dialect, func(t *testing.T) {
			t.Parallel()
			factory, ok := dialectHostFactory[dialect]
			if !ok {
				t.Fatalf("no host factory for dialect %q", dialect)
			}
			host := factory(t)
			fn(t, host)
		})
	}
}

// ForEachDialectMap returns a map of enabled dialect names to core.Host
// instances. Use this when a test needs to reference specific dialects in
// assertions or select a dialect by name at runtime (e.g., table-driven
// tests where the dialect is an input to the test function).
//
// The returned hosts each have their own database and are cleaned up when
// the test finishes.
//
//	hosts := plugintest.ForEachDialectMap(t)
//	for dialect, host := range hosts {
//	    t.Run(dialect, func(t *testing.T) {
//	        ctx := context.Background()
//	        s := NewStore(host)
//	        item, err := s.Create(ctx, "test")
//	        require.NoError(t, err)
//	    })
//	}
//
// This is an escape hatch for unusual patterns. Most tests should prefer
// ForEachDialect, which is simpler and avoids the map iteration boilerplate.
func ForEachDialectMap(t *testing.T) map[string]core.Host {
	t.Helper()
	hosts := make(map[string]core.Host, len(supportedDialects))
	for _, dialect := range supportedDialects {
		if !testdb.ShouldTest(dialect) {
			continue
		}
		factory, ok := dialectHostFactory[dialect]
		if !ok {
			t.Fatalf("no host factory for dialect %q", dialect)
		}
		hosts[dialect] = factory(t)
	}
	if len(hosts) == 0 {
		t.Skip("no dialects enabled (CI_DIALECT restriction)")
	}
	return hosts
}

// DialectNames returns the list of dialect names currently enabled by the
// CI_DIALECT environment variable. Use this in test setup to pre-allocate
// per-dialect state or decide which tests to skip.
//
//	func TestSetup(t *testing.T) {
//	    for _, d := range plugintest.DialectNames() {
//	        t.Logf("dialect %s is active", d)
//	    }
//	}
func DialectNames() []string {
	var active []string
	for _, d := range supportedDialects {
		if testdb.ShouldTest(d) {
			active = append(active, d)
		}
	}
	return active
}

// NewPluginHost creates a test host for the given dialect with plugin-
// specific migrations applied on top of the core migrations. This is a
// convenience for test suites that need a fully-migrated host in a single
// call instead of the usual "factory then MigratePlugin" two-step.
//
// The migrationFS parameter is typed as fs.FS so both embed.FS and
// fstest.MapFS work, enabling tests to inject fake migration files:
//
//	migrationFS := fstest.MapFS{
//	    "psql/001_init.up.sql":   &fstest.MapFile{Data: []byte("...")},
//	    "mysql/001_init.up.sql":  &fstest.MapFile{Data: []byte("...")},
//	    "mssql/001_init.up.sql":  &fstest.MapFile{Data: []byte("...")},
//	}
//	host := plugintest.NewPluginHost(t, "postgres", migrationFS, "plugin_my_migrations")
//
// Pass nil for migrationFS or empty string for migrationTable to skip
// plugin migration (equivalent to calling the host factory directly).
//
// When dialect is "disabled" by CI_DIALECT, the test is skipped via t.Skip.
func NewPluginHost(t *testing.T, dialect string, migrationFS fs.FS, migrationTable string) core.Host {
	t.Helper()
	if !testdb.ShouldTest(dialect) {
		t.Skipf("dialect %q disabled by CI_DIALECT", dialect)
	}
	factory, ok := dialectHostFactory[dialect]
	if !ok {
		t.Fatalf("unsupported dialect %q", dialect)
	}
	host := factory(t)
	if migrationFS != nil && migrationTable != "" {
		if err := MigratePlugin(context.Background(), host, migrationFS, migrationTable); err != nil {
			t.Fatalf("NewPluginHost migrate %s: %v", dialect, err)
		}
	}
	return host
}
