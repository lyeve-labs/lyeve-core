// Package testutil provides a 2-tenant integration test harness for verifying
// cross-tenant IDOR isolation across all plugins. Spins up a Postgres
// testcontainer (shared per test binary), creates per-test databases, and
// provides two tenant contexts. Call NewHarness(t), then RunMigrations and
// exercise store methods from each tenant context to confirm tenant_id
// filtering.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	otelnoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/jackc/pgx/v5/stdlib" // register pgx database/sql driver

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	// Assigns core.PluginMigrate and core.PluginMigrateRollback from its init.
	_ "github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Harness is a reusable 2-tenant integration test environment backed by a
// real Postgres testcontainer.
//
// It provides:
//   - A per-test Postgres database (isolated via CREATE DATABASE)
//   - A minimal Host implementation wired to the database
//   - Two tenant contexts (TenantA, TenantB) carrying distinct tenant IDs
//   - Helpers for migrations, seeding, and row counting
//
// The tenant contexts carry IDs via core.WithTenantID. Plugin stores
// that read tenant_id from context (via core.TenantIDFromCtx) will see
// the correct tenant. The harness does NOT add automatic tenant filtering:
// that's the job of the plugin store code being tested.
type Harness struct {
	// DB is the raw *sql.DB connected to the test database.
	DB *sql.DB

	// Host is a minimal core.Host wired to DB. Plugin stores can use
	// host.Querier(ctx) and host.Dialect() as they normally would.
	Host core.Host

	// TenantA and TenantB are context.Context values carrying distinct
	// tenant IDs via core.WithTenantID. Pass these as the ctx argument
	// to plugin store methods to simulate requests from different tenants.
	TenantA context.Context
	TenantB context.Context

	// TenantIDA and TenantIDB are the slug strings ("tenant_a", "tenant_b").
	TenantIDA string
	TenantIDB string
}

// Shared container (one per test binary)

var pgContainer struct {
	ctr      *postgres.PostgresContainer
	host     string
	port     string
	adminDSN string
	ready    bool
	err      error
}

func ensureContainer(t *testing.T) {
	t.Helper()
	if pgContainer.ready {
		return
	}
	if pgContainer.err != nil {
		t.Fatalf("shared pg container previously failed: %v", pgContainer.err)
	}

	ctx := context.Background()
	pgContainer.ctr, pgContainer.err = postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_testutil"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if pgContainer.err != nil {
		t.Fatalf("start pg container: %v", pgContainer.err)
	}

	// Get the real connection string from the container (includes actual
	// credentials). testcontainers-go masks the password in ConnectionString
	// output, but the returned string uses the real credentials for pgx.
	baseDSN, err := pgContainer.ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		pgContainer.err = err
		t.Fatalf("pg connection string: %v", err)
	}

	pgContainer.host, pgContainer.err = pgContainer.ctr.Host(ctx)
	if pgContainer.err != nil {
		t.Fatalf("pg host: %v", pgContainer.err)
	}

	mp, err := pgContainer.ctr.MappedPort(ctx, "5432")
	if err != nil {
		pgContainer.err = err
		t.Fatalf("pg mapped port: %v", err)
	}
	pgContainer.port = mp.Port()

	// Build admin DSN by replacing the database name in the base DSN.
	// This preserves the real credentials (not the masked placeholder).
	pgContainer.adminDSN = replaceDatabase(baseDSN, "postgres")

	pgContainer.ready = true
}

// NewHarness

// NewHarness creates a fresh test database in the shared PG container and
// returns a Harness with two tenant contexts. The database and all
// connections are cleaned up when the test finishes.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	ensureContainer(t)

	ctx := context.Background()

	dbName := fmt.Sprintf("harness_%d", time.Now().UnixNano())
	adminDB, err := sql.Open("pgx", pgContainer.adminDSN)
	if err != nil {
		t.Fatalf("harness admin open: %v", err)
	}
	defer adminDB.Close()

	_, err = adminDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbName))
	if err != nil {
		t.Fatalf("create harness db %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanup, err2 := sql.Open("pgx", pgContainer.adminDSN)
		if err2 != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.ExecContext(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = cleanup.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
	})

	// Connect to the per-test database by swapping the database name
	// in the admin DSN (which carries real credentials).
	dbDSN := replaceDatabase(pgContainer.adminDSN, dbName)

	db, err := sql.Open("pgx", dbDSN)
	if err != nil {
		t.Fatalf("harness open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("harness ping: %v", err)
	}

	host := &testHost{db: db, dialect: "postgres"}

	return &Harness{
		DB:        db,
		Host:      host,
		TenantA:   core.WithTenantID(context.Background(), "tenant_a"),
		TenantB:   core.WithTenantID(context.Background(), "tenant_b"),
		TenantIDA: "tenant_a",
		TenantIDB: "tenant_b",
	}
}

// Migration helper

// RunMigrations applies plugin-level migrations via core.PluginMigrate.
//
// core.PluginMigrate is a function variable that pkg/plugin assigns from its
// init, so a caller that never pulls that package in calls nil and panics
// rather than failing. This package is imported by tests that have no reason
// to import pkg/plugin themselves, so the blank import above does it for
// them, and the guard below says which wiring is missing if that import ever
// goes away.
func (h *Harness) RunMigrations(t *testing.T, migrationsFS fs.FS, trackingTable string) {
	t.Helper()
	if core.PluginMigrate == nil {
		t.Fatal("core.PluginMigrate is not wired: import lyeve-core/pkg/plugin for its init")
	}
	if err := core.PluginMigrate(context.Background(), h.DB, "postgres", migrationsFS, trackingTable); err != nil {
		t.Fatalf("plugin migrate (%s): %v", trackingTable, err)
	}
}

// SetupTenants creates the tenant schemas in the test database. Only needed
// when testing schema-per-tenant behavior. For sys_* table IDOR tests (which
// live in the public schema), this is optional.
func (h *Harness) SetupTenants(t *testing.T, tenantIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for _, tid := range tenantIDs {
		schema := "tenant_" + tid
		_, err := h.DB.ExecContext(ctx,
			fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS "%s"`, strings.ReplaceAll(schema, `"`, `""`)))
		if err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}
}

// Seed helpers

// SeedRow inserts a single row into the given table. Columns and values are
// parallel slices. Include tenant_id explicitly if the table has one.
func (h *Harness) SeedRow(t *testing.T, tenantID, table string, cols []string, vals []any) {
	t.Helper()
	ctx := core.WithTenantID(context.Background(), tenantID)

	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))

	_, err := h.Host.Querier(ctx).Exec(ctx, query, vals...)
	if err != nil {
		t.Fatalf("seed %s (tenant=%s): %v", table, tenantID, err)
	}
}

// SeedRowSQL executes raw SQL in the context of the given tenant.
func (h *Harness) SeedRowSQL(t *testing.T, tenantID, query string, args ...any) {
	t.Helper()
	ctx := core.WithTenantID(context.Background(), tenantID)
	_, err := h.Host.Querier(ctx).Exec(ctx, query, args...)
	if err != nil {
		t.Fatalf("seed sql (tenant=%s): %v", tenantID, err)
	}
}

// Query helpers

// CountRows returns the row count for a query in the given tenant context.
func (h *Harness) CountRows(t *testing.T, tenantID, table, where string, args ...any) int {
	t.Helper()
	ctx := core.WithTenantID(context.Background(), tenantID)

	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)
	if where != "" {
		query += " WHERE " + where
	}
	var count int
	row, qErr := h.Host.Querier(ctx).QueryRow(ctx, query, args...)
	if qErr != nil {
		t.Fatalf("query %s (tenant=%s): %v", table, tenantID, qErr)
	}
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count %s (tenant=%s): %v", table, tenantID, err)
	}
	return count
}

// Minimal test Host implementation

type testHost struct {
	db      *sql.DB
	dialect string
}

func (h *testHost) Querier(_ context.Context) core.Querier   { return &sqlQuerier{db: h.db} }
func (h *testHost) QuerierRO(_ context.Context) core.Querier { return &sqlQuerier{db: h.db} }
func (h *testHost) RawDB() *sql.DB                           { return h.db }
func (h *testHost) MigrationDB() *sql.DB                     { return h.db }
func (h *testHost) Dialect() string                          { return h.dialect }
func (h *testHost) Logger(_ context.Context) *slog.Logger    { return slog.Default() }
func (h *testHost) Config() core.Config                      { return nil }
func (h *testHost) Hooks() core.HookBus                      { return nil }
func (h *testHost) HookPublisher() core.HookPublisher        { return nil }
func (h *testHost) Version() string                          { return "test" }
func (h *testHost) Schema() core.SchemaEngine                { return nil }
func (h *testHost) Tracer(_ string) trace.Tracer             { return otelnoop.NewTracerProvider().Tracer("test") }
func (h *testHost) Secret(key string) (val string, found bool) {
	if key == "encryption_key" {
		return "test-enc-key-not-for-production-use", true
	}
	return "", false
}
func (h *testHost) Secrets(key string) (vals []string, found bool) {
	return nil, false
}

func (h *testHost) WorkerPool() *engine.WorkerPool               { return nil }
func (h *testHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (h *testHost) ParallelEngine() *engine.ParallelEngine       { return nil }
func (h *testHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (h *testHost) DistLock(name string) *engine.DistLock        { return nil }

func (h *testHost) Capabilities() core.CapabilitySet {
	return core.CapabilitySet{
		Features: map[string]bool{},
		Plan:     "free",
		State:    "free",
	}
}

func (h *testHost) HasFeature(ctx context.Context, feature string) bool { return false }

type sqlQuerier struct {
	db *sql.DB
}

func (q *sqlQuerier) QueryRow(ctx context.Context, sqlStr string, args ...any) (core.Row, error) {
	return q.db.QueryRowContext(ctx, sqlStr, args...), nil
}

func (q *sqlQuerier) Query(ctx context.Context, sqlStr string, args ...any) (core.Rows, error) {
	rows, err := q.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	return &rowsWrapper{rows: rows}, nil
}

func (q *sqlQuerier) Exec(ctx context.Context, sqlStr string, args ...any) (core.CommandTag, error) {
	res, err := q.db.ExecContext(ctx, sqlStr, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

func (q *sqlQuerier) Begin(ctx context.Context) (core.Tx, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &txWrapper{tx: tx}, nil
}

type rowsWrapper struct{ rows *sql.Rows }

func (r *rowsWrapper) Next() bool             { return r.rows.Next() }
func (r *rowsWrapper) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *rowsWrapper) Close()                 { r.rows.Close() }
func (r *rowsWrapper) Err() error             { return r.rows.Err() }

type txWrapper struct{ tx *sql.Tx }

func (t *txWrapper) QueryRow(ctx context.Context, sqlStr string, args ...any) (core.Row, error) {
	return t.tx.QueryRowContext(ctx, sqlStr, args...), nil
}
func (t *txWrapper) Query(ctx context.Context, sqlStr string, args ...any) (core.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	return &rowsWrapper{rows: rows}, nil
}
func (t *txWrapper) Exec(ctx context.Context, sqlStr string, args ...any) (core.CommandTag, error) {
	res, err := t.tx.ExecContext(ctx, sqlStr, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}
func (t *txWrapper) Begin(_ context.Context) (core.Tx, error) {
	return nil, fmt.Errorf("nested transactions not supported in test host")
}
func (t *txWrapper) Commit(_ context.Context) error   { return t.tx.Commit() }
func (t *txWrapper) Rollback(_ context.Context) error { return t.tx.Rollback() }

// replaceDatabase swaps the database name in a Postgres connection URL.
// Given DSN ".../olddb?sslmode=disable", it returns ".../newdb?sslmode=disable".
func replaceDatabase(dsn, newDB string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + newDB
	return u.String()
}
