//go:build integration && !mutest
// +build integration,!mutest

// Package core_test: ScopedHost integration tests against real PostgreSQL.
// Verifies capability-based access control end-to-end: a ScopedHost with
// limited caps truly blocks access to DB, secrets, raw DB handles, and hooks.
// Complements the unit tests in scoped_host_test.go (package core).
package core_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Test DB helpers

func pgForScopedHost(t *testing.T) *sql.DB {
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

// seedScopedHostTables creates sys_users, sys_secrets, and a content table
// with test data so deny/allow paths exercise real database queries.
func seedScopedHostTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS sys_users (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			email TEXT NOT NULL,
			password_hash TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS sys_secrets (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
			key TEXT NOT NULL,
			value TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS content_pages (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			tenant_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
			title TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT ''
		)`,
	} {
		_, err := db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	// Seed one sys user, one sys secret, and one content row.
	_, err := db.ExecContext(ctx, `INSERT INTO sys_users (email, password_hash) VALUES ('admin@test.local', 'hash1')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sys_secrets (key, value) VALUES ('encryption_key', 'secret-key-123')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO content_pages (tenant_id, title, body) VALUES ('00000000-0000-0000-0000-000000000000', 'Welcome', 'Hello world')`)
	require.NoError(t, err)
}

// Minimal Host backed by a real *sql.DB

// testHost implements core.Host for integration tests.
// Querier and RawDB are backed by a real PG connection.
// Config returns canned values. Other methods are stubs.
type testHost struct {
	db      *sql.DB
	dialect string
	cfg     *testConfig
}

func newTestHost(db *sql.DB) *testHost {
	return &testHost{
		db:      db,
		dialect: "postgres",
		cfg:     newTestConfig(),
	}
}

func (h *testHost) Querier(_ context.Context) core.Querier   { return &sqlDBQuerier{db: h.db} }
func (h *testHost) QuerierRO(_ context.Context) core.Querier { return &sqlDBQuerier{db: h.db} }
func (h *testHost) RawDB() *sql.DB                           { return h.db }
func (h *testHost) MigrationDB() *sql.DB                     { return h.db }
func (h *testHost) Logger(_ context.Context) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
func (h *testHost) Tracer(name string) trace.Tracer            { return noop.NewTracerProvider().Tracer(name) }
func (h *testHost) Config() core.Config                        { return h.cfg }
func (h *testHost) Hooks() core.HookBus                        { return stubHookBus{} }
func (h *testHost) HookPublisher() core.HookPublisher          { return stubHookPublisher{} }
func (h *testHost) Version() string                            { return "v0.0.0-test" }
func (h *testHost) Dialect() string                            { return h.dialect }
func (h *testHost) Schema() core.SchemaEngine                  { return nil } // stubbed. Not exercised
func (h *testHost) WorkerPool() *core.WorkerPool               { return nil }
func (h *testHost) GoroutineTracker() *core.GoroutineTracker   { return nil }
func (h *testHost) ParallelEngine() *core.ParallelEngine       { return nil }
func (h *testHost) AsyncHookExecutor() *core.AsyncHookExecutor { return nil }
func (h *testHost) DistLock(name string) *core.DistLock        { return nil }
func (h *testHost) Capabilities() core.CapabilitySet {
	return core.CapabilitySet{Features: map[string]bool{}, Plan: "free", State: "free"}
}
func (h *testHost) HasFeature(ctx context.Context, feature string) bool { return false }

// sqlDBQuerier wraps *sql.DB as core.Querier

type sqlDBQuerier struct{ db *sql.DB }

func (q *sqlDBQuerier) QueryRow(ctx context.Context, sQL string, args ...any) (core.Row, error) {
	return q.db.QueryRowContext(ctx, sQL, args...), nil
}

func (q *sqlDBQuerier) Query(ctx context.Context, sQL string, args ...any) (core.Rows, error) {
	rows, err := q.db.QueryContext(ctx, sQL, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsAdapter{rows: rows}, nil
}

func (q *sqlDBQuerier) Exec(ctx context.Context, sQL string, args ...any) (core.CommandTag, error) {
	res, err := q.db.ExecContext(ctx, sQL, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

func (q *sqlDBQuerier) Begin(ctx context.Context) (core.Tx, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlDBTx{tx: tx}, nil
}

type sqlDBTx struct{ tx *sql.Tx }

func (t *sqlDBTx) QueryRow(ctx context.Context, sQL string, args ...any) (core.Row, error) {
	return t.tx.QueryRowContext(ctx, sQL, args...), nil
}
func (t *sqlDBTx) Query(ctx context.Context, sQL string, args ...any) (core.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, sQL, args...)
	if err != nil {
		return nil, err
	}
	return &sqlRowsAdapter{rows: rows}, nil
}
func (t *sqlDBTx) Exec(ctx context.Context, sQL string, args ...any) (core.CommandTag, error) {
	res, err := t.tx.ExecContext(ctx, sQL, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}
func (t *sqlDBTx) Begin(_ context.Context) (core.Tx, error) {
	return nil, fmt.Errorf("nested transactions not supported by test host")
}
func (t *sqlDBTx) Commit(_ context.Context) error   { return t.tx.Commit() }
func (t *sqlDBTx) Rollback(_ context.Context) error { return t.tx.Rollback() }

// sqlRowsAdapter wraps *sql.Rows as core.Rows.
// sql.Rows.Close() returns error, but core.Rows.Close() does not.
type sqlRowsAdapter struct{ rows *sql.Rows }

func (a *sqlRowsAdapter) Next() bool             { return a.rows.Next() }
func (a *sqlRowsAdapter) Scan(dest ...any) error { return a.rows.Scan(dest...) }
func (a *sqlRowsAdapter) Close()                 { _ = a.rows.Close() }
func (a *sqlRowsAdapter) Err() error             { return a.rows.Err() }

// Stub Config

type testConfig struct {
	strings map[string]string
}

func newTestConfig() *testConfig {
	return &testConfig{
		strings: map[string]string{
			"jwt_secret":     "jwt-test-secret-value",
			"encryption_key": "encryption-test-key-value",
			"app_name":       "LyEve Test",
			"debug":          "true",
		},
	}
}

func (c *testConfig) String(key string) string        { return c.strings[key] }
func (c *testConfig) Bool(key string) bool            { return c.strings[key] == "true" || c.strings[key] == "1" }
func (c *testConfig) Duration(_ string) time.Duration { return 0 }
func (c *testConfig) Strings(key string) []string {
	if v, ok := c.strings[key]; ok && v != "" {
		return []string{v}
	}
	return nil
}

// Stubs

type stubHookBus struct{}

func (stubHookBus) Subscribe(_ string, _ core.EventType, _ core.EventHandler) core.Subscription {
	return stubSubscription{}
}

func (stubHookBus) On(_ string, _ core.SystemEventHandler) core.Subscription {
	return stubSubscription{}
}

type stubSubscription struct{}

func (stubSubscription) Unsubscribe() {}

type stubHookPublisher struct{}

func (stubHookPublisher) Publish(_ context.Context, _ core.Event) error { return nil }

// Integration Tests

// TestScopedHost_FailClosed verifies that zero caps denies everything:
// Querier returns deniedQuerier (ErrCapDenied on all ops), RawDB returns nil,
// Config hides secret keys, Hooks/HookPublisher return denied stubs.
func TestScopedHost_FailClosed(t *testing.T) {
	db := pgForScopedHost(t)
	seedScopedHostTables(t, db)
	inner := newTestHost(db)
	sh := core.NewScopedHost(inner, "no-caps-plugin", 0)
	ctx := context.Background()

	// Querier: returns deniedQuerier. All methods fail with ErrCapDenied.
	q := sh.Querier(ctx)
	_, err := q.Query(ctx, "SELECT 1")
	assert.ErrorIs(t, err, core.ErrCapDenied, "Querier().Query")
	_, err = q.Exec(ctx, "SELECT 1")
	assert.ErrorIs(t, err, core.ErrCapDenied, "Querier().Exec")
	_, err = q.Begin(ctx)
	assert.ErrorIs(t, err, core.ErrCapDenied, "Querier().Begin")
	var n int
	row, qrErr := q.QueryRow(ctx, "SELECT 1")
	require.NoError(t, qrErr, "QueryRow")
	row.Scan(&n)

	// QuerierRO: same denied path.
	qro := sh.QuerierRO(ctx)
	_, err = qro.Query(ctx, "SELECT 1")
	assert.ErrorIs(t, err, core.ErrCapDenied, "QuerierRO().Query")

	// RawDB: nil.
	assert.Nil(t, sh.RawDB(), "RawDB must be nil without CapRawDB")
	assert.Nil(t, sh.MigrationDB(), "MigrationDB must be nil without CapRawDB")

	// Config: secret keys hidden.
	assert.Empty(t, sh.Config().String("jwt_secret"), "jwt_secret hidden")
	assert.Empty(t, sh.Config().String("encryption_key"), "encryption_key hidden")
	assert.Nil(t, sh.Config().Strings("jwt_secrets"), "jwt_secrets hidden")

	// Non-secret keys pass through.
	assert.Equal(t, "LyEve Test", sh.Config().String("app_name"))

	// Hooks: denied.
	sh.Hooks().Subscribe("content", core.AfterCreate, func(_ context.Context, _ core.Event) error {
		return nil
	}).Unsubscribe() // denied subscription. Must not panic

	err = sh.HookPublisher().Publish(ctx, core.Event{Type: core.AfterCreate})
	assert.ErrorIs(t, err, core.ErrCapDenied, "HookPublisher().Publish")

	// Always-allowed methods still work.
	assert.Equal(t, "v0.0.0-test", sh.Version())
	assert.Equal(t, "postgres", sh.Dialect())
	assert.NotNil(t, sh.Logger(ctx))
	assert.NotNil(t, sh.Tracer("test"))
}

// TestScopedHost_CapDBRead verifies that CapDBRead grants a real Querier
// that can read from system and content tables. RawDB, secrets, and hooks
// remain denied because those require separate caps.
func TestScopedHost_CapDBRead(t *testing.T) {
	db := pgForScopedHost(t)
	seedScopedHostTables(t, db)
	inner := newTestHost(db)
	sh := core.NewScopedHost(inner, "readonly-plugin", core.CapDBRead)
	ctx := context.Background()

	// Query sys_users: real DB read.
	rows, err := sh.Querier(ctx).Query(ctx, "SELECT email FROM sys_users")
	require.NoError(t, err)
	defer rows.Close()
	var emails []string
	for rows.Next() {
		var e string
		require.NoError(t, rows.Scan(&e))
		emails = append(emails, e)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, emails, "admin@test.local")

	// Query sys_secrets: real DB read.
	rows2, err := sh.Querier(ctx).Query(ctx, "SELECT key, value FROM sys_secrets")
	require.NoError(t, err)
	defer rows2.Close()
	var keys []string
	for rows2.Next() {
		var k, v string
		require.NoError(t, rows2.Scan(&k, &v))
		keys = append(keys, k)
	}
	require.NoError(t, rows2.Err())
	assert.Contains(t, keys, "encryption_key")

	// QueryRow on content table.
	var title string
	row, qrErr := sh.Querier(ctx).QueryRow(ctx, "SELECT title FROM content_pages LIMIT 1")
	require.NoError(t, qrErr, "QueryRow")
	row.Scan(&title)
	assert.Equal(t, "Welcome", title)

	// RawDB still nil.
	assert.Nil(t, sh.RawDB(), "RawDB must be nil without CapRawDB")

	// Config secrets hidden.
	assert.Empty(t, sh.Config().String("jwt_secret"))
	assert.Empty(t, sh.Config().String("encryption_key"))

	// Hooks denied.
	err = sh.HookPublisher().Publish(ctx, core.Event{Type: core.AfterCreate})
	assert.ErrorIs(t, err, core.ErrCapDenied)
}

// TestScopedHost_CapDBWrite verifies CapDBWrite grants database read+write,
// but other caps (RawDB, ConfigSecret) remain denied.
func TestScopedHost_CapDBWrite(t *testing.T) {
	db := pgForScopedHost(t)
	seedScopedHostTables(t, db)
	inner := newTestHost(db)
	sh := core.NewScopedHost(inner, "rw-plugin", core.CapDBWrite)
	ctx := context.Background()

	// Write works.
	tag, err := sh.Querier(ctx).Exec(ctx,
		"INSERT INTO content_pages (title, body) VALUES ($1, $2)", "New Page", "content")
	require.NoError(t, err)
	assert.Equal(t, int64(1), tag.RowsAffected)

	// Read works (CapDBWrite implies CapDBRead).
	rows, err := sh.Querier(ctx).Query(ctx, "SELECT title FROM content_pages ORDER BY title")
	require.NoError(t, err)
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var title string
		require.NoError(t, rows.Scan(&title))
		titles = append(titles, title)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, titles, "New Page")

	// RawDB still nil.
	assert.Nil(t, sh.RawDB(), "RawDB must be nil without CapRawDB")

	// Secrets still hidden.
	assert.Empty(t, sh.Config().String("jwt_secret"))
}

// TestScopedHost_CapRawDB verifies RawDB/MigrationDB return non-nil when
// CapRawDB is granted, and nil without it.
func TestScopedHost_CapRawDB(t *testing.T) {
	db := pgForScopedHost(t)
	inner := newTestHost(db)

	shWith := core.NewScopedHost(inner, "rawdb-plugin", core.CapRawDB)
	assert.NotNil(t, shWith.RawDB(), "RawDB non-nil with CapRawDB")
	assert.NotNil(t, shWith.MigrationDB(), "MigrationDB non-nil with CapRawDB")

	shWithout := core.NewScopedHost(inner, "no-rawdb-plugin", core.CapDBRead)
	assert.Nil(t, shWithout.RawDB(), "RawDB nil without CapRawDB")
}

// TestScopedHost_CapConfigSecret verifies secret config keys are visible
// when CapConfigSecret is granted, and hidden (empty) without it.
func TestScopedHost_CapConfigSecret(t *testing.T) {
	db := pgForScopedHost(t)
	inner := newTestHost(db)

	shWith := core.NewScopedHost(inner, "secret-plugin", core.CapConfigSecret)
	assert.Equal(t, "jwt-test-secret-value", shWith.Config().String("jwt_secret"))
	assert.Equal(t, "encryption-test-key-value", shWith.Config().String("encryption_key"))
	assert.Equal(t, "LyEve Test", shWith.Config().String("app_name"), "non-secret passes through")

	shWithout := core.NewScopedHost(inner, "nosecret-plugin", 0)
	assert.Empty(t, shWithout.Config().String("jwt_secret"))
	assert.Empty(t, shWithout.Config().String("encryption_key"))
	assert.Equal(t, "LyEve Test", shWithout.Config().String("app_name"))
	assert.Nil(t, shWithout.Config().Strings("jwt_secrets"))
}

// TestScopedHost_CapHooks verifies hooks are available with CapHooks,
// denied without.
func TestScopedHost_CapHooks(t *testing.T) {
	db := pgForScopedHost(t)
	inner := newTestHost(db)
	ctx := context.Background()

	shWith := core.NewScopedHost(inner, "hooks-plugin", core.CapHooks)
	sub := shWith.Hooks().Subscribe("content", core.AfterCreate, func(_ context.Context, _ core.Event) error {
		return nil
	})
	sub.Unsubscribe() // real subscription. Must not panic

	shWithout := core.NewScopedHost(inner, "nohooks-plugin", 0)
	shWithout.Hooks().Subscribe("content", core.AfterCreate, func(_ context.Context, _ core.Event) error {
		return nil
	}).Unsubscribe() // denied subscription. Must not panic

	err := shWithout.HookPublisher().Publish(ctx, core.Event{Type: core.AfterCreate})
	assert.ErrorIs(t, err, core.ErrCapDenied)
}

// TestScopedHost_CapCombined verifies multiple caps work together:
// DBRead + ConfigSecret -> can query + read secrets, but RawDB and hooks denied.
func TestScopedHost_CapCombined(t *testing.T) {
	db := pgForScopedHost(t)
	seedScopedHostTables(t, db)
	inner := newTestHost(db)
	sh := core.NewScopedHost(inner, "combo-plugin", core.CapDBRead|core.CapConfigSecret)
	ctx := context.Background()

	// Reads work.
	rows, err := sh.Querier(ctx).Query(ctx, "SELECT email FROM sys_users")
	require.NoError(t, err)
	rows.Close()

	// Secrets accessible.
	assert.Equal(t, "jwt-test-secret-value", sh.Config().String("jwt_secret"))

	// RawDB denied.
	assert.Nil(t, sh.RawDB())

	// Hooks denied.
	err = sh.HookPublisher().Publish(ctx, core.Event{Type: core.AfterCreate})
	assert.ErrorIs(t, err, core.ErrCapDenied)
}

// TestScopedHost_PluginRegistry_UnregisteredReturnsZero verifies PluginCaps
// returns 0 for unregistered plugins, resulting in full denial when used
// with NewScopedHost.
func TestScopedHost_PluginRegistry_UnregisteredReturnsZero(t *testing.T) {
	caps := core.PluginCaps("no-such-plugin")
	assert.Equal(t, core.Capability(0), caps)
}

// TestScopedHost_PluginRegistry_UndeclaredGetsCapAll verifies that PluginCaps
// reports CapAll for a plugin registered via RegisterPlugin: a plugin that
// declared nothing narrows no policy ceiling. The grant it runs with is
// plugin.CapPolicy's, not this value.
func TestScopedHost_PluginRegistry_UndeclaredGetsCapAll(t *testing.T) {
	core.RegisterPlugin("undeclared-integration-plugin", func() core.Plugin {
		return &noopPlugin{name: "undeclared-integration-plugin"}
	})
	caps := core.PluginCaps("undeclared-integration-plugin")
	assert.True(t, caps.Has(core.CapAll), "PluginCaps reports CapAll for a plugin that declared nothing")
}

// TestScopedHost_PluginRegistry_ExplicitCaps verifies RegisterPluginWithCaps
// stores and retrieves only the declared caps.
func TestScopedHost_PluginRegistry_ExplicitCaps(t *testing.T) {
	core.RegisterPluginWithCaps("explicit-int-plugin", func() core.Plugin {
		return &noopPlugin{name: "explicit-int-plugin"}
	}, core.CapDBRead|core.CapConfigSecret)

	caps := core.PluginCaps("explicit-int-plugin")
	assert.True(t, caps.Has(core.CapDBRead), "should have CapDBRead")
	assert.True(t, caps.Has(core.CapConfigSecret), "should have CapConfigSecret")
	assert.False(t, caps.Has(core.CapRawDB), "should NOT have CapRawDB")
	assert.False(t, caps.Has(core.CapHooks), "should NOT have CapHooks")
}

// noopPlugin is a minimal Plugin implementation for registry tests.
type noopPlugin struct{ name string }

func (p *noopPlugin) Name() string                               { return p.name }
func (p *noopPlugin) Start(_ context.Context, _ core.Host) error { return nil }
func (p *noopPlugin) Stop(_ context.Context) error               { return nil }
