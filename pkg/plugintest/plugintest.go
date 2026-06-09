// Package plugintest provides a real-DB test harness for CMS plugins. It spins
// up testcontainers (PostgreSQL, MySQL, MSSQL), returns a core.Host, and
// supplies hook spies, HTTP recorders, database fixtures, multi-dialect
// runners (ForEachDialect), and contract-fixture loading via mockhost.
package plugintest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// SkipDialect skips the test when CI_DIALECT is set and does not match dialect.
func SkipDialect(t *testing.T, dialect string) {
	t.Helper()
	if !testdb.ShouldTest(dialect) {
		t.Skipf("CI_DIALECT=%s (skipping %s)", os.Getenv("CI_DIALECT"), dialect)
	}
}

// HostOption configures a testHost created by NewHost.
type HostOption func(*testHost)

// WithHooks sets the HookBus on the test host. When nil (default), Hooks()
// returns a no-op implementation.
func WithHooks(hooks core.HookBus) HostOption {
	return func(h *testHost) {
		h.hooks = hooks
	}
}

// WithConfig sets a custom Config on the test host. When nil (default), the
// host returns a staticConfig with test defaults.
func WithConfig(cfg core.Config) HostOption {
	return func(h *testHost) {
		h.cfg = cfg
	}
}

// WithVersion sets the engine version string returned by Host.Version().
func WithVersion(version string) HostOption {
	return func(h *testHost) {
		h.version = version
	}
}

// WithSchema sets the SchemaEngine returned by Host.Schema().
func WithSchema(schema core.SchemaEngine) HostOption {
	return func(h *testHost) {
		h.schema = schema
	}
}

// WithCapabilities sets the CapabilitySet returned by Host.Capabilities().
// When not set, the default grants every registered plugin's name and every
// name a registered plugin declares.
func WithCapabilities(caps core.CapabilitySet) HostOption {
	return func(h *testHost) {
		h.caps = &caps
	}
}

// dbPool is the minimal interface needed from a database pool.
// db.DB (internal/db) satisfies this interface.
type dbPool interface {
	Engine() string
	SQLDB() *sql.DB
	QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error)
	Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (sql.Result, error)
	Begin(ctx context.Context) (*sql.Tx, error)
}

// NewHost creates a core.Host from an existing database pool. The pool must
// satisfy SQLDB() and Engine() (as testdb pools do). Combine with HostOption
// functions (WithHooks, WithConfig, etc.) to customize behavior.
//
// pool may be nil, which yields a host with no database and the postgres
// dialect. Use it for paths that return before touching storage, such as a
// plugin Start that stops at an unmet license gate.
func NewHost(t T, pool dbPool, opts ...HostOption) core.Host {
	h := &testHost{
		pool:    pool,
		dialect: "postgres",
		version: "test",
	}
	if pool != nil {
		h.rawDB = pool.SQLDB()
		h.dialect = pool.Engine()
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Unlicensed returns a host whose capability set grants no features, which is
// what a plugin sees when its entitlement is absent. Plugins gate on
// Capabilities().Features[name]. A nil map reports false for every name.
func Unlicensed(t T) core.Host {
	return NewHost(t, nil, WithCapabilities(core.CapabilitySet{Plan: "free", State: "free"}))
}

// testHost implements core.Host with a dbPool + *sql.DB pair and a fixed
// config. Plugins call Start(ctx, host) which reads Dialect() and RawDB().
type testHost struct {
	pool    dbPool
	rawDB   *sql.DB
	dialect string
	hooks   core.HookBus
	cfg     core.Config
	schema  core.SchemaEngine
	version string
	storage core.Storage
	caps    *core.CapabilitySet

	// configSections is the registry a plugin's Start registers its
	// configuration section in, as it does on the engine host.
	configSections core.ConfigSectionRegistry

	// Created on first cached read. Several constructors build a testHost
	// literal, so this cannot be a constructor field.
	cacheOnce  sync.Once
	queryCache *db.QueryCache
}

// Plugin stores reach their cached read path by type-asserting the host to
// this interface. A test host that does not satisfy it silently sends every
// plugin suite down the uncached fallback instead.
var _ core.QueryCacheProvider = (*testHost)(nil)

// A plugin that owns sys_ or generated content tables reaches its engine-bound
// connection by asserting this. A test host that does not satisfy it sends
// every plugin suite down a different path from the one production takes.
var _ core.EngineDBConnProvider = (*testHost)(nil)

// Querier returns a core.Querier backed by the database pool.
func (h *testHost) Querier(ctx context.Context) core.Querier {
	return &querierAdapter{pool: h.pool}
}

// QuerierRO returns the same pool as Querier. No replica is configured in tests.
func (h *testHost) QuerierRO(ctx context.Context) core.Querier {
	return h.Querier(ctx)
}

// Dialect returns the engine dialect (e.g. "postgres", "mysql", "mssql").
func (h *testHost) Dialect() string { return h.dialect }

// RawDB returns the underlying *sql.DB for direct database access.
func (h *testHost) RawDB() *sql.DB { return h.rawDB }

// MigrationDB returns the same *sql.DB as RawDB for migration use.
func (h *testHost) MigrationDB() *sql.DB { return h.rawDB }

// EngineDBConn returns a connection bound to the engine's own database, doing
// the same re-bind the engine host does.
//
// A test pool owns one database, but a plugin proving it survives a stranded
// connection binds one to a tenant itself, so returning the connection as it
// came would hand that connection straight back. A test host that takes a
// different path from production reads as covered while proving nothing.
func (h *testHost) EngineDBConn(ctx context.Context) (*sql.Conn, error) {
	if h.rawDB == nil {
		return nil, errors.New("engine database connection: no pool on this host")
	}
	pool, ok := h.pool.(db.DB)
	if !ok && h.dialect != "postgres" {
		return nil, fmt.Errorf("engine database connection: the pool on this host cannot name its own database, so a connection cannot be bound to it on %s", h.dialect)
	}
	conn, err := h.rawDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine database connection: %w", err)
	}
	if ok {
		if err := db.PinConnToEngineDB(ctx, pool, conn); err != nil {
			// A failed re-bind leaves the session on the database the caller
			// was trying to get off, and Close would return it to the pool in
			// that state.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			return nil, fmt.Errorf("engine database connection: %w", err)
		}
	}
	return conn, nil
}

// Version returns the host version string.
func (h *testHost) Version() string { return h.version }

// Hooks returns the configured HookBus or a no-op implementation.
func (h *testHost) Hooks() core.HookBus {
	if h.hooks != nil {
		return h.hooks
	}
	return &noopHookBus{}
}

// HookPublisher returns a HookPublisher from the configured HookBus or a
// no-op implementation.
func (h *testHost) HookPublisher() core.HookPublisher {
	if h.hooks != nil {
		if pub, ok := h.hooks.(core.HookPublisher); ok {
			return pub
		}
	}
	return &noopHookBus{}
}

// Storage returns the backend set via SetStorage, or nil if unconfigured.
func (h *testHost) Storage() core.Storage { return h.storage }

// SetStorage injects a storage backend for tests (e.g. in-memory memStorage).
func (h *testHost) SetStorage(s core.Storage) { h.storage = s }

// Schema returns the configured SchemaEngine, or nil.
func (h *testHost) Schema() core.SchemaEngine { return h.schema }

// RegisterSchemaEngine implements core.SchemaEngineRegistrar, which the plugin
// that owns schema definitions calls in Start with its engine and in Stop with
// nil.
//
// A test host that did not satisfy this would fail that plugin's Start
// outright, and every other plugin's suite would run against a host that
// cannot accept an engine while production can. A double that implements less
// than the real thing sends every suite down a path production does not take.
func (h *testHost) RegisterSchemaEngine(e core.SchemaEngine) { h.schema = e }

// SchemaSource implements core.SchemaSourceProvider, answering the registry
// the content path reads. It is whatever engine was registered or supplied,
// and no engine at all otherwise.
func (h *testHost) SchemaSource() core.SchemaSource {
	if h.schema != nil {
		return core.SchemaSourceOf(h.schema)
	}
	return core.AbsentSchemaSource{}
}

var (
	_ core.SchemaEngineRegistrar = (*testHost)(nil)
	_ core.SchemaSourceProvider  = (*testHost)(nil)
)

// Tracer returns a no-op tracer.
func (h *testHost) Tracer(name string) trace.Tracer { return &noop.Tracer{} }

// Secret delegates to the underlying config's SecretsProvider, if it implements one.
func (h *testHost) Secret(key string) (val string, found bool) {
	cfg := h.Config()
	if sp, ok := cfg.(interface {
		Secret(key string) (val string, found bool)
	}); ok {
		return sp.Secret(key)
	}
	return "", false
}

// Secrets delegates to the underlying config's SecretsProvider, if it implements one.
func (h *testHost) Secrets(key string) (vals []string, found bool) {
	cfg := h.Config()
	if sp, ok := cfg.(interface {
		Secrets(key string) (vals []string, found bool)
	}); ok {
		return sp.Secrets(key)
	}
	return nil, false
}

// CachedFetch implements core.QueryCacheProvider with the same read-through
// cache the engine wires at boot, so a plugin suite exercises the caching its
// store actually ships rather than a stub. Writes that do not invalidate show
// up here as a stale read, which is what they are in production too.
func (h *testHost) CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn core.CacheFetcher) (bool, error) {
	key := db.BuildCacheKey(pluginName, dialect, tenantID, sql, args)
	return h.cache().FetchInto(ctx, key, ttl, dest, fn)
}

// InvalidateCache implements core.QueryCacheProvider.InvalidateCache.
func (h *testHost) InvalidateCache(prefix string) int {
	return h.cache().Invalidate(prefix)
}

// cache returns the host's query result cache, sized as the engine sizes it.
func (h *testHost) cache() *db.QueryCache {
	h.cacheOnce.Do(func() {
		h.queryCache = db.NewQueryCache(1000, 30*time.Second)
	})
	return h.queryCache
}

// Logger returns a logger that writes warnings to stderr.
func (h *testHost) Logger(ctx context.Context) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// Config returns the host config, defaulting to a staticConfig with test values.
func (h *testHost) Config() core.Config {
	if h.cfg != nil {
		return h.cfg
	}
	return defaultConfig()
}

// defaultConfig is the configuration a test host answers when no option set
// one.
func defaultConfig() *staticConfig {
	return &staticConfig{
		jwtSecret:         "test-secret-32-bytes-minimum!!!",
		jwtSecrets:        []string{"test-secret-32-bytes-minimum!!!"},
		encryptionKey:     "test-enc-key-not-for-production-use",
		passwordHashAlgo:  "bcrypt",
		jwtExpirySecs:     3600,
		corsOrigins:       []string{"*"},
		rateLimitRPS:      0,
		maxBodyBytes:      1 << 20,
		apiListen:         ":0",
		adminListen:       ":0",
		storageDriver:     "local",
		storagePath:       "/tmp/cms-test-storage",
		sessionCookieName: security.SessionCookieNameInsecure,
	}
}

func (h *testHost) WorkerPool() *engine.WorkerPool               { return nil }
func (h *testHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (h *testHost) ParallelEngine() *engine.ParallelEngine       { return nil }
func (h *testHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (h *testHost) DistLock(name string) *engine.DistLock        { return nil }

func (h *testHost) Capabilities() core.CapabilitySet {
	if h.caps != nil {
		return *h.caps
	}
	// Default: what a build that links no licensing implementation grants.
	return defaultCapabilities()
}

func (h *testHost) HasFeature(ctx context.Context, feature string) bool { return true }

// RegisterConfigSection implements core.ConfigSectionRegistrar, so a plugin
// that offers its configuration to a bundle registers it here the way it does
// on the engine host.
func (h *testHost) RegisterConfigSection(name string, s core.ConfigSection) {
	h.configSections.RegisterConfigSection(name, s)
}

// RegisterOwnedConfigSection implements core.ConfigSectionOwnerRegistrar,
// which a scoped host wrapping this one registers through.
func (h *testHost) RegisterOwnedConfigSection(owner, name string, s core.ConfigSection) error {
	return h.configSections.RegisterOwnedConfigSection(owner, name, s)
}

// ConfigSections implements core.ConfigSectionProvider.
func (h *testHost) ConfigSections() []core.ConfigSection {
	return h.configSections.ConfigSections()
}

var (
	_ core.ConfigSectionRegistrar      = (*testHost)(nil)
	_ core.ConfigSectionOwnerRegistrar = (*testHost)(nil)
	_ core.ConfigSectionProvider       = (*testHost)(nil)
)

type noopHookBus struct{}

// defaultCapabilities returns the capability set of a build that links no
// licensing implementation: every registered plugin's name, and every name a
// registered plugin declares through core.FeatureDeclarer, derived rather than
// hand-kept.
//
// Deriving it matters. A harness that granted a name no build grants would
// let a plugin pass in tests on a gate it can never open in production, and a
// harness that missed one would start the plugin degraded, so the harness
// grants exactly what that build grants. A plugin that gates on a name it
// neither owns nor declares reads as unlicensed here, as it would there.
func defaultCapabilities() core.CapabilitySet {
	names := core.CompiledFeatureNames()
	feats := make(map[string]bool, len(names))
	for _, f := range names {
		feats[f] = true
	}
	return core.CapabilitySet{
		Features: feats,
		Plan:     "free",
		State:    "free",
	}
}

// Subscribe returns a no-op subscription that does nothing.
func (n *noopHookBus) Subscribe(schema string, event core.EventType, handler core.EventHandler) core.Subscription {
	return &noopSubscription{}
}

// On returns a no-op subscription that does nothing.
func (n *noopHookBus) On(event string, handler core.SystemEventHandler) core.Subscription {
	return &noopSubscription{}
}

// Publish is a no-op that never returns an error.
func (n *noopHookBus) Publish(ctx context.Context, event core.Event) error {
	return nil
}

type noopSubscription struct{}

// Unsubscribe is a no-op for the noop subscription.
func (n *noopSubscription) Unsubscribe() {}

type querierAdapter struct{ pool dbPool }

// QueryRow rewrites placeholders and delegates to the pool.
func (q *querierAdapter) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	sql, args = db.RewritePlaceholders(sql, q.pool.Engine(), args)
	row, err := q.pool.QueryRow(ctx, sql, args...)
	if err != nil {
		return core.ErrorRow(err), err
	}
	return row, nil
}

// Query rewrites placeholders and delegates to the pool.
func (q *querierAdapter) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	sql, args = db.RewritePlaceholders(sql, q.pool.Engine(), args)
	rows, err := q.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &rowsAdapter{rows: rows}, nil
}

// Exec rewrites placeholders and delegates to the pool.
func (q *querierAdapter) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	sql, args = db.RewritePlaceholders(sql, q.pool.Engine(), args)
	tag, err := q.pool.Exec(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := tag.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

// Begin starts a database transaction via the pool.
func (q *querierAdapter) Begin(ctx context.Context) (core.Tx, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txAdapter{tx: tx, dialect: q.pool.Engine()}, nil
}

type rowsAdapter struct{ rows *sql.Rows }

// Next advances to the next database row.
func (r *rowsAdapter) Next() bool { return r.rows.Next() }

// Scan copies the current row's columns into the destination pointers.
func (r *rowsAdapter) Scan(dest ...any) error { return r.rows.Scan(dest...) }

// Close closes the underlying sql.Rows.
func (r *rowsAdapter) Close() { _ = r.rows.Close() }

// Err returns any error accumulated during iteration.
func (r *rowsAdapter) Err() error { return r.rows.Err() }

type txAdapter struct {
	tx      *sql.Tx
	dialect string
}

// QueryRow rewrites placeholders and queries within the transaction.
func (t *txAdapter) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	sql, args = db.RewritePlaceholders(sql, t.dialect, args)
	return t.tx.QueryRowContext(ctx, sql, args...), nil
}

// Query rewrites placeholders and runs a query within the transaction.
func (t *txAdapter) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	sql, args = db.RewritePlaceholders(sql, t.dialect, args)
	rows, err := t.tx.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &rowsAdapter{rows: rows}, nil
}

// Exec rewrites placeholders and executes a statement within the transaction.
func (t *txAdapter) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	sql, args = db.RewritePlaceholders(sql, t.dialect, args)
	tag, err := t.tx.ExecContext(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := tag.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

// Begin returns an error: nested transactions are not supported.
func (t *txAdapter) Begin(ctx context.Context) (core.Tx, error) {
	return nil, fmt.Errorf("nested transactions not supported")
}

// Commit commits the underlying database transaction.
func (t *txAdapter) Commit(ctx context.Context) error { return t.tx.Commit() }

// Rollback rolls back the underlying database transaction.
func (t *txAdapter) Rollback(ctx context.Context) error { return t.tx.Rollback() }

type staticConfig struct {
	jwtSecret         string
	jwtSecrets        []string
	encryptionKey     string
	passwordHashAlgo  string
	jwtExpirySecs     int
	corsOrigins       []string
	rateLimitRPS      int
	maxBodyBytes      int
	apiListen         string
	adminListen       string
	storageDriver     string
	storagePath       string
	sessionCookieName string
	secureCookie      bool
	multiTenant       bool
	baseURL           string
	// consoleURL replaces TestConsoleURL once consoleURLSet says an option
	// chose it, which is how a test models an install that left it unset.
	consoleURL    string
	consoleURLSet bool
}

// TestConsoleURL is the console URL a test host answers unless WithConsoleURL
// chose another. A plugin that mails a link to the console refuses to start in
// production without one, and a test host is production unless APP_ENV says
// otherwise, so the default host models an install that set it.
const TestConsoleURL = mockhost.TestConsoleURL

// WithConsoleURL sets console_url on the test host. An empty value models an
// install that never set LYEVE_CONSOLE_URL.
func WithConsoleURL(consoleURL string) HostOption {
	return func(h *testHost) {
		if h.cfg == nil {
			h.cfg = defaultConfig()
		}
		if cfg, ok := h.cfg.(*staticConfig); ok {
			cfg.consoleURL = consoleURL
			cfg.consoleURLSet = true
		}
	}
}

// WithMultiTenant sets multi_tenant on the test host so plugin code that
// branches on it can exercise the multi-tenant path.
func WithMultiTenant(enabled bool) HostOption {
	return func(h *testHost) {
		if h.cfg == nil {
			h.cfg = defaultConfig()
		}
		if cfg, ok := h.cfg.(*staticConfig); ok {
			cfg.multiTenant = enabled
		}
	}
}

// WithBaseURL sets base_url on the test host for OAuth callback and redirect
// URI computation.
func WithBaseURL(baseURL string) HostOption {
	return func(h *testHost) {
		if h.cfg == nil {
			h.cfg = defaultConfig()
		}
		if cfg, ok := h.cfg.(*staticConfig); ok {
			cfg.baseURL = baseURL
		}
	}
}

// String implements core.Config for known config keys.
func (c *staticConfig) String(key string) string {
	switch key {
	case "jwt_secret":
		return c.jwtSecret
	case "jwt_expiry_secs":
		return fmt.Sprintf("%d", c.jwtExpirySecs)
	case "password_hash_algo":
		return c.passwordHashAlgo
	case "api_listen_addr":
		return c.apiListen
	case "admin_listen_addr":
		return c.adminListen
	case "storage_driver":
		return c.storageDriver
	case "storage_local_path":
		return c.storagePath
	case "session_cookie_name":
		return c.sessionCookieName
	case "encryption_key":
		return c.encryptionKey
	case "base_url":
		return c.baseURL
	case core.ConfigKeyConsoleURL:
		if c.consoleURLSet {
			return c.consoleURL
		}
		return TestConsoleURL
	case "jwt_secrets":
		// Comma-joined for callers that split.
		out := ""
		for i, s := range c.jwtSecrets {
			if i > 0 {
				out += ","
			}
			out += s
		}
		return out
	default:
		return pluginKeyFromEnv(key)
	}
}

// pluginKeyFromEnv answers a key this harness has no field for, the way the
// engine's own host does: an unknown plugin key resolves through the layered
// configuration resolver, whose highest layer is the process environment.
// Without it a plugin's own settings would be invisible under test, and a
// plugin could only test them by reading the environment directly instead of
// going through the host.
func pluginKeyFromEnv(key string) string {
	if key == "" {
		return ""
	}
	return os.Getenv(strings.ToUpper(key))
}

// Bool implements core.Config for boolean config keys.
func (c *staticConfig) Bool(key string) bool {
	if key == "secure_cookie" {
		return c.secureCookie
	}
	if key == "multi_tenant" {
		return c.multiTenant
	}
	// Derived, as the engine derives it: the adapter answers is_production from
	// cfg.IsProduction(), which lowercases APP_ENV and treats unset as
	// production. A bare IS_PRODUCTION lookup would let a test set
	// APP_ENV=production and still be told it was not, so a test of a
	// production-only path would never reach it.
	if key == "is_production" {
		return IsProductionEnv()
	}
	b, err := strconv.ParseBool(pluginKeyFromEnv(key))
	return err == nil && b
}

// Duration implements core.Config. Returns 0 for all keys.
func (c *staticConfig) Duration(key string) time.Duration { return 0 }

// Strings implements core.Config for list-valued config keys.
func (c *staticConfig) Strings(key string) []string {
	switch key {
	case "cors_origins":
		out := make([]string, len(c.corsOrigins))
		copy(out, c.corsOrigins)
		return out
	case "jwt_secrets":
		out := make([]string, len(c.jwtSecrets))
		copy(out, c.jwtSecrets)
		return out
	}
	raw := c.String(key)
	if raw == "" {
		return nil
	}
	// Split comma-separated for fallback keys.
	parts := splitAndTrim(raw, ",")
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// Secret returns encryption_key or jwt_secret from the static config.
func (c *staticConfig) Secret(key string) (val string, found bool) {
	switch key {
	case "encryption_key":
		if c.encryptionKey != "" {
			return c.encryptionKey, true
		}
		return "", false
	case "jwt_secret":
		if c.jwtSecret != "" {
			return c.jwtSecret, true
		}
		return "", false
	}
	// A plugin's own credential, which this harness has no field for. The
	// engine answers these from the same resolver, so a test can set one with
	// t.Setenv and the plugin reads it exactly as it would in production.
	if v := pluginKeyFromEnv(key); v != "" {
		return v, true
	}
	return "", false
}

// Secrets returns jwt_secrets from the static config.
func (c *staticConfig) Secrets(key string) (vals []string, found bool) {
	if key == "jwt_secrets" {
		if len(c.jwtSecrets) > 0 {
			return c.Strings("jwt_secrets"), true
		}
		return nil, false
	}
	return nil, false
}

func splitAndTrim(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i:i+len(sep)] == sep {
			part := trimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + len(sep)
			if i < len(s) {
				i += len(sep) - 1
			}
		}
	}
	return out
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// Postgres spins up a postgres:16-alpine container, runs core migrations,
// and returns a core.Host. The container is terminated on test cleanup.
func Postgres(t *testing.T) core.Host {
	t.Helper()
	SkipDialect(t, "postgres")
	return NewHost(t, testdb.Postgres(t))
}

// PostgresDSN returns the DSN of a fresh database on the shared container,
// with the engine's migrations applied, for a test that boots the whole
// engine against it rather than handing one plugin a host. The database is
// dropped when the test ends.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	SkipDialect(t, "postgres")
	_, dsn := testdb.PostgresWithDSN(t)
	return dsn
}

// PostgresWithOptions is like Postgres but accepts HostOption values.
func PostgresWithOptions(t *testing.T, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "postgres")
	return NewHost(t, testdb.Postgres(t), opts...)
}

// MySQL spins up a mysql:8 container, runs core migrations, and returns a
// core.Host. The container is terminated on test cleanup.
func MySQL(t *testing.T) core.Host {
	t.Helper()
	SkipDialect(t, "mysql")
	pool := testdb.MySQL(t)
	return &testHost{
		pool:    pool,
		rawDB:   pool.SQLDB(),
		dialect: pool.Engine(),
		version: "test",
	}
}

// MySQLWithOptions is like MySQL but accepts HostOption values.
func MySQLWithOptions(t *testing.T, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "mysql")
	return NewHost(t, testdb.MySQL(t), opts...)
}

// MSSQL spins up an Azure SQL Edge container, runs core migrations, and
// returns a core.Host. The container is terminated on test cleanup.
func MSSQL(t *testing.T) core.Host {
	t.Helper()
	SkipDialect(t, "mssql")
	pool := testdb.MSSQL(t)
	return &testHost{
		pool:    pool,
		rawDB:   pool.SQLDB(),
		dialect: pool.Engine(),
		version: "test",
	}
}

// MSSQLWithOptions is like MSSQL but accepts HostOption values.
func MSSQLWithOptions(t *testing.T, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "mssql")
	return NewHost(t, testdb.MSSQL(t), opts...)
}

// Concurrency
//
// Every host above gets a pool of testdb.DefaultMaxConns connections, which is
// small on purpose: many suites share one container per dialect and a large
// default starves them.
//
// That cap is invisible from inside a test and it bounds concurrency directly.
// A burst of 20 goroutines against a 4-connection pool is four callers in the
// database and sixteen queued on the pool, so a race that needs more than four
// simultaneous sessions never happens and the test passes without reproducing
// anything. Reach for these constructors when the behavior under test IS the
// concurrency: lock contention, deadlock retries, upsert races.
//
// Size it to the burst the test issues, and no larger.

// ConcurrentPostgres returns a Postgres host whose pool admits maxConns
// simultaneous sessions.
func ConcurrentPostgres(t *testing.T, maxConns int, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "postgres")
	return NewHost(t, testdb.PostgresWithMaxConns(t, maxConns), opts...)
}

// ConcurrentMySQL returns a MySQL host whose pool admits maxConns simultaneous
// sessions.
func ConcurrentMySQL(t *testing.T, maxConns int, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "mysql")
	return NewHost(t, testdb.MySQLWithMaxConns(t, maxConns), opts...)
}

// ConcurrentMSSQL returns an MSSQL host whose pool admits maxConns simultaneous
// sessions.
func ConcurrentMSSQL(t *testing.T, maxConns int, opts ...HostOption) core.Host {
	t.Helper()
	SkipDialect(t, "mssql")
	return NewHost(t, testdb.MSSQLWithMaxConns(t, maxConns), opts...)
}

var _ core.WriteSerializer = (*testHost)(nil)

// SerializeWrite implements core.WriteSerializer with the lock the engine
// takes, over the test database, so a plugin suite proves its ceiling holds
// under concurrent writes. A host with no database runs fn as it is.
func (h *testHost) SerializeWrite(ctx context.Context, key string, fn func(context.Context) error) error {
	pool, ok := h.pool.(db.DB)
	if !ok {
		return fn(ctx)
	}
	return db.SerializeWrite(ctx, pool, key, fn)
}

var _ core.AdminSeatGuardProvider = (*testHost)(nil)

// AdminSeatGuard implements core.AdminSeatGuardProvider with the guard the
// engine wires, over the test database and this host's capability set, so a
// plugin suite proves its seat refusal against the count production makes.
// It reads no memberships, as an engine running no membership plugin does,
// and a host with no database answers nil.
func (h *testHost) AdminSeatGuard() core.AdminSeatGuard {
	pool, ok := h.pool.(db.DB)
	if !ok {
		return nil
	}
	return db.NewAdminSeatGuard(db.NewUserStore(pool),
		func() int { return h.Capabilities().Limit(core.CapAdminSeats) }, nil)
}
