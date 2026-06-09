package plugintest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// LifecycleConfig configures the standard plugin lifecycle integration test
// suite. Provide a factory and expected migration tables. The harness runs
// start/stop/migrate/routes subtests and optional AdditionalSubTests.
type LifecycleConfig struct {
	// PluginFactory creates a fresh plugin instance for each subtest.
	// Must be callable multiple times. Must not share state between calls.
	PluginFactory func() core.Plugin

	// MigrationTables are the table names expected to exist after Start().
	// The harness uses DBFixture.AssertTableExists for each.
	MigrationTables []string

	// StartIdempotentSuccess controls the expected behavior of the second
	// Start() call. When true (default), the second Start must succeed.
	// When false, the second Start must return an error.
	StartIdempotentSuccess bool

	// RouteCount is the exact number of routes expected. When nil, the
	// route count is not checked.
	RouteCount *int

	// RouteMethodCounts maps HTTP methods (GET, POST, PUT, DELETE) to
	// expected counts. When nil, method distribution is not checked.
	RouteMethodCounts map[string]int

	// RouteGroup is the expected route group. Defaults to plugin.GroupAdmin
	// when empty. Set SkipRouteGroupCheck for plugins with mixed groups.
	RouteGroup core.RouteGroup

	// SkipRouteGroupCheck disables per-route group validation. Use this
	// when the plugin declares routes with mixed groups: the default
	// single-group check would incorrectly fail on the non-matching routes.
	SkipRouteGroupCheck bool

	// RoutesBeforeStart: when true, Routes() returns nil before Start().
	// When false, Routes() is not checked before Start. Defaults to true.
	RoutesBeforeStart bool

	// AdditionalSubTests runs after the standard lifecycle subtests with a
	// started plugin. The plugin is stopped automatically after the callback
	// returns via t.Cleanup.
	AdditionalSubTests func(t *testing.T, ctx context.Context, host core.Host, p core.Plugin)

	// Setup runs before any subtests. Use this for things like seeding
	// prerequisite tables that the plugin's migration queries depend on.
	// The harness calls t.Fatalf if Setup returns an error.
	Setup func(ctx context.Context, host core.Host) error
}

// PtrInt returns a pointer to v, for use with LifecycleConfig.RouteCount.
func PtrInt(v int) *int { return &v }

// RunLifecycle runs the standard plugin lifecycle subtests (start, idempotent
// start, migrate, routes, stop, additional) against the given host. Each
// subtest gets a fresh plugin instance from PluginFactory.
func RunLifecycle(t *testing.T, host core.Host, cfg LifecycleConfig) {
	t.Helper()
	ctx := context.Background()

	if cfg.PluginFactory == nil {
		t.Fatal("LifecycleConfig.PluginFactory is required")
	}

	// Normalize defaults.
	if cfg.RouteGroup == "" {
		cfg.RouteGroup = core.GroupAdmin
	}

	// Setup
	if cfg.Setup != nil {
		if err := cfg.Setup(ctx, host); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	// 1. Start
	t.Run("start", func(t *testing.T) {
		p := cfg.PluginFactory()
		if p == nil {
			t.Fatal("PluginFactory returned nil")
		}
		if err := p.Start(ctx, host); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	// 2. Start idempotency
	t.Run("start_idempotent", func(t *testing.T) {
		p := cfg.PluginFactory()
		if err := p.Start(ctx, host); err != nil {
			t.Fatalf("first Start: %v", err)
		}
		defer func() { _ = p.Stop(ctx) }() // err suppressed: test teardown, best-effort

		err := p.Start(ctx, host)
		if cfg.StartIdempotentSuccess {
			if err != nil {
				t.Fatalf("second Start: %v", err)
			}
		} else {
			if err == nil {
				t.Fatal("expected error on second Start, got nil")
			}
		}
	})

	// 3. Migration
	if len(cfg.MigrationTables) > 0 {
		t.Run("migrate", func(t *testing.T) {
			p := cfg.PluginFactory()
			if err := p.Start(ctx, host); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = p.Stop(ctx) }() // err suppressed: test teardown, best-effort

			db := NewDBFixture(t, host)
			for _, table := range cfg.MigrationTables {
				db.AssertTableExists(ctx, table)
			}
		})
	}

	// 4. Routes
	t.Run("routes", func(t *testing.T) {
		p := cfg.PluginFactory()

		// Check Routes() before Start if configured.
		if cfg.RoutesBeforeStart {
			if routes := pluginRoutes(p); routes != nil {
				t.Errorf("expected nil Routes() before Start, got %d routes", len(routes))
			}
		}

		if err := p.Start(ctx, host); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = p.Stop(ctx) }() // err suppressed: test teardown, best-effort

		routes := pluginRoutes(p)

		// Route count check.
		if cfg.RouteCount != nil {
			got := len(routes)
			if got != *cfg.RouteCount {
				t.Errorf("expected %d routes, got %d", *cfg.RouteCount, got)
			}
		}

		// Validate individual routes.
		for _, r := range routes {
			if r.Handler == nil {
				t.Errorf("route %s %s has nil handler", r.Method, r.Pattern)
			}
			if !cfg.SkipRouteGroupCheck && cfg.RouteGroup != "" && r.Group != cfg.RouteGroup {
				t.Errorf("route %s %s: expected group %v, got %v",
					r.Method, r.Pattern, cfg.RouteGroup, r.Group)
			}
		}

		// Method distribution check.
		if cfg.RouteMethodCounts != nil {
			methods := map[string]int{}
			for _, r := range routes {
				methods[r.Method]++
			}
			for method, want := range cfg.RouteMethodCounts {
				if methods[method] != want {
					t.Errorf("route count for %s: got %d, want %d", method, methods[method], want)
				}
			}
		}
	})

	// 5. Stop idempotency
	t.Run("stop", func(t *testing.T) {
		p := cfg.PluginFactory()
		if err := p.Start(ctx, host); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Errorf("first Stop: %v", err)
		}
		if err := p.Stop(ctx); err != nil {
			t.Errorf("second Stop: %v", err)
		}
	})

	// 6. Plugin-specific subtests
	if cfg.AdditionalSubTests != nil {
		p := cfg.PluginFactory()
		if err := p.Start(ctx, host); err != nil {
			t.Fatalf("Start for additional tests: %v", err)
		}
		// Use Cleanup to ensure Stop is called even if AdditionalSubTests
		// panics or fails.
		t.Cleanup(func() {
			_ = p.Stop(context.Background())
		})
		cfg.AdditionalSubTests(t, ctx, host, p)
	}
}

// StartPlugin calls p.Start and fails the test on error. Registers a Cleanup
// that calls p.Stop.
func StartPlugin(t *testing.T, ctx context.Context, host core.Host, p core.Plugin) {
	t.Helper()
	if err := p.Start(ctx, host); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Stop(context.Background())
	})
}

// AssertTablesExist fails if any named table does not exist. Queries
// information_schema.tables for dialect portability.
func AssertTablesExist(t T, ctx context.Context, host core.Host, tables ...string) {
	t.Helper()
	for _, table := range tables {
		var count int
		row, err := host.Querier(ctx).QueryRow(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1`,
			table)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if err := row.Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("expected table %q to exist, got count=%d", table, count)
		}
	}
}

// RequireRoutes checks basic route invariants: non-nil handler and correct group.
func RequireRoutes(t *testing.T, routes []core.RouteDecl, wantGroup core.RouteGroup) {
	t.Helper()
	for _, r := range routes {
		if r.Handler == nil {
			t.Errorf("route %s %s has nil handler", r.Method, r.Pattern)
		}
		if wantGroup != "" && r.Group != wantGroup {
			t.Errorf("route %s %s: expected group %v, got %v", r.Method, r.Pattern, wantGroup, r.Group)
		}
	}
}

// MigrationAssertions groups common migration-table checks.
type MigrationAssertions struct {
	t    *testing.T
	ctx  context.Context
	host core.Host
}

// NewMigrationAssertions returns assertions scoped to the test, ctx, and host.
func NewMigrationAssertions(t *testing.T, ctx context.Context, host core.Host) *MigrationAssertions {
	t.Helper()
	return &MigrationAssertions{t: t, ctx: ctx, host: host}
}

// AssertTableExists fails if the named migration table does not exist.
func (m *MigrationAssertions) AssertTableExists(tableName string) {
	AssertMigrationTable(m.t, m.ctx, m.host, tableName)
}

// AssertEntries fails if the migration table does not contain exactly want rows.
func (m *MigrationAssertions) AssertEntries(tableName string, want int) {
	AssertMigrationCount(m.t, m.ctx, m.host, tableName, want)
}

// RawDB returns host.RawDB() or fails the test. Use only when direct *sql.DB
// access is needed (e.g. seeding prerequisite tables before migration).
func RawDB(t *testing.T, host core.Host) *sql.DB {
	t.Helper()
	db := host.RawDB()
	if db == nil {
		t.Fatal("host.RawDB() is nil - host was not set up with a database")
	}
	return db
}

// pluginRoutes returns the plugin's routes via type assertion to RoutesPlugin,
// or nil if the plugin doesn't implement that interface.
func pluginRoutes(p core.Plugin) []core.RouteDecl {
	if rp, ok := p.(core.RoutesPlugin); ok {
		return rp.Routes()
	}
	return nil
}
