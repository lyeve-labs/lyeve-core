// Package mockhost provides a unit-test-friendly core.Host implementation
// requiring no real database. Use for testing handler logic, route
// declarations, config consumption, and hook interactions without
// testcontainers. Key components: New() (defaults), MockQuerier (canned
// data), MockConfig (key/value), AssertCalled/AssertNotCalled (verification).
package mockhost

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	// The plugin registry answers core.CompiledFeatureNames only once the
	// package that keeps it is linked, and a test binary that reaches this
	// host through nothing else would otherwise grant nothing.
	_ "github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// T is the minimal testing interface used by mockhost assertions.
// *testing.T, *testing.B, and *testing.F all satisfy this interface.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// New creates a fresh MockHost with a HookSpy and default Config.
func New(t T) *MockHost {
	return &MockHost{
		t:     t,
		spy:   &QuerierSpy{},
		conf:  NewMockConfig(),
		hooks: &hookSpy{},
	}
}

// MockHost is the top-level test harness. It produces a core.Host
// via Host(), which wraps MockQuerier, MockConfig, and HookSpy.
type MockHost struct {
	t              T
	spy            *QuerierSpy
	conf           *MockConfig
	hooks          *hookSpy
	hasFeatureFunc func(ctx context.Context, feature string) bool
}

// Spy returns the QuerierSpy for setting up expectations and inspecting calls.
func (m *MockHost) Spy() *QuerierSpy { return m.spy }

// Config returns the MockConfig for setting up config key/value pairs.
func (m *MockHost) Config() *MockConfig { return m.conf }

// Hooks returns the hook spy for inspecting hook subscriptions and publishes.
func (m *MockHost) Hooks() *hookSpy { return m.hooks }

// SetHasFeature configures the HasFeature callback. When non-nil it is
// called for every HasFeature check. When nil (default) the adapter
// returns false.
func (m *MockHost) SetHasFeature(fn func(ctx context.Context, feature string) bool) {
	m.hasFeatureFunc = fn
}

// Host returns a core.Host implementation backed by this MockHost.
func (m *MockHost) Host() core.Host {
	return &mockHostAdapter{m: m}
}

// QuerierSpy

// CallRecord captures a single Querier method invocation.
type CallRecord struct {
	Method string // "QueryRow", "Query", "Exec", "Begin"
	SQL    string
	Args   []any
}

// RowExpectation defines the response for an expected QueryRow call.
type RowExpectation struct {
	SQL       string
	ScanVals  []any // values to return from Scan
	ScanErr   error // error to return from Scan
	Called    bool  // set after matching call
	callCount int   // how many times to match
}

// WithScan sets the values to return from Scan().
func (e *RowExpectation) WithScan(vals ...any) *RowExpectation { e.ScanVals = vals; return e }

// WithError sets an error to return from Scan.
func (e *RowExpectation) WithError(err error) *RowExpectation { e.ScanErr = err; return e }

// QueryExpectation defines the response for an expected Query call.
type QueryExpectation struct {
	SQL    string
	Rows   *MockRows
	Err    error
	Called bool
}

// ReturnRows sets the rows to return from Query.
func (e *QueryExpectation) ReturnRows(rows *MockRows) *QueryExpectation { e.Rows = rows; return e }

// ReturnError sets an error to return from Query.
func (e *QueryExpectation) ReturnError(err error) *QueryExpectation { e.Err = err; return e }

// ExecExpectation defines the response for an expected Exec call.
type ExecExpectation struct {
	SQL    string
	Tag    core.CommandTag
	Err    error
	Called bool
}

// ReturnTag sets the CommandTag to return from Exec.
func (e *ExecExpectation) ReturnTag(tag core.CommandTag) *ExecExpectation { e.Tag = tag; return e }

// ReturnError sets an error to return from Exec.
func (e *ExecExpectation) ReturnError(err error) *ExecExpectation { e.Err = err; return e }

// QuerierSpy records Querier method calls and serves canned responses.
// Set up expectations before calling handlers, then assert on calls after.
//
// Usage:
//
//	spy := mock.Spy()
//	spy.OnQueryRow("SELECT id FROM users WHERE email = $1",
//	    mock.Row().WithScan(uuid.New()))
//	spy.OnExec("UPDATE users SET name = $1 WHERE id = $2",
//	    mock.ExecResult(1))
//	spy.OnQuery("SELECT * FROM bookmarks ORDER BY created_at DESC",
//	    mock.Rows(cols("id","title"), row("abc","Hello"), row("def","World")))
type QuerierSpy struct {
	mu        sync.RWMutex
	Calls     []CallRecord
	ExpectRow []*RowExpectation
	ExpectQ   []*QueryExpectation
	ExpectE   []*ExecExpectation
}

// OnQueryRow sets up an expectation for a QueryRow call matching SQL.
// The returned RowExpectation's WithScan/WithError methods configure the response.
// The expectation matches exactly once. Call Again() to match multiple times.
func (s *QuerierSpy) OnQueryRow(sql string) *RowExpectation {
	e := &RowExpectation{SQL: sql, callCount: 1}
	s.mu.Lock()
	s.ExpectRow = append(s.ExpectRow, e)
	s.mu.Unlock()
	return e
}

// OnQuery sets up an expectation for a Query call matching SQL.
func (s *QuerierSpy) OnQuery(sql string) *QueryExpectation {
	e := &QueryExpectation{SQL: sql}
	s.mu.Lock()
	s.ExpectQ = append(s.ExpectQ, e)
	s.mu.Unlock()
	return e
}

// OnExec sets up an expectation for an Exec call matching SQL.
func (s *QuerierSpy) OnExec(sql string) *ExecExpectation {
	e := &ExecExpectation{SQL: sql}
	s.mu.Lock()
	s.ExpectE = append(s.ExpectE, e)
	s.mu.Unlock()
	return e
}

// AssertCalled fails the test if a call matching method and sql was not made.
func (s *QuerierSpy) AssertCalled(t T, method, sql string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.Calls {
		if c.Method == method && c.SQL == sql {
			return
		}
	}
	t.Errorf("expected Querier.%s call with SQL %q, but it was not made", method, sql)
}

// AssertNotCalled fails the test if a call matching method and sql was made.
func (s *QuerierSpy) AssertNotCalled(t T, method, sql string) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.Calls {
		if c.Method == method && c.SQL == sql {
			t.Errorf("expected Querier.%s call with SQL %q NOT to be made, but it was", method, sql)
			return
		}
	}
}

// CallCount returns the number of calls recorded for the given method.
func (s *QuerierSpy) CallCount(method string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, c := range s.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Reset clears all recorded calls and expectations.
func (s *QuerierSpy) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = nil
	s.ExpectRow = nil
	s.ExpectQ = nil
	s.ExpectE = nil
}

// MockRow

// MockRow implements core.Row for canned scan values.
type MockRow struct {
	vals  []any
	scanN int
	err   error
}

// Row creates a new MockRow. Call WithScan or WithError to configure.
func Row() *MockRow { return &MockRow{} }

// WithScan sets the values to return from Scan() calls.
func (r *MockRow) WithScan(vals ...any) *MockRow { r.vals = vals; return r }

// WithError sets an error to return from Scan().
func (r *MockRow) WithError(err error) *MockRow { r.err = err; return r }

// Scan copies vals into dest pointers. Returns the configured error.
func (r *MockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i < len(r.vals) {
			switch dp := d.(type) {
			case *int:
				if v, ok := r.vals[i].(int); ok {
					*dp = v
				}
			case *string:
				if v, ok := r.vals[i].(string); ok {
					*dp = v
				}
			case *bool:
				if v, ok := r.vals[i].(bool); ok {
					*dp = v
				}
			case *int64:
				if v, ok := r.vals[i].(int64); ok {
					*dp = v
				}
			case *float64:
				if v, ok := r.vals[i].(float64); ok {
					*dp = v
				}
			case *time.Time:
				if v, ok := r.vals[i].(time.Time); ok {
					*dp = v
				}
			case *uuid.UUID:
				if v, ok := r.vals[i].(uuid.UUID); ok {
					*dp = v
				}
			case core.UUIDScanner:
				// Dialect-aware UUID scanner (core.ScanUUID). Delegate to its
				// Scan so MSSQL's byte-order correction is exercised. Accept the
				// test convenience uuid.UUID by normalizing it to its string form.
				v := r.vals[i]
				if u, ok := v.(uuid.UUID); ok {
					v = u.String()
				}
				if err := dp.Scan(v); err != nil {
					return err
				}
			case core.NullUUIDScanner:
				v := r.vals[i]
				if u, ok := v.(uuid.UUID); ok {
					v = u.String()
				}
				if err := dp.Scan(v); err != nil {
					return err
				}
			default:
				// For interface{} or other types, use a type-assert copy.
				// This covers *interface{} from test setups.
			}
		}
	}
	r.scanN++
	return nil
}

// MockRows

// MockRows implements core.Rows with a pre-baked row set.
type MockRows struct {
	cols   []string
	data   [][]any
	pos    int
	err    error
	closed bool
}

// Rows creates a new MockRows. Chain .Cols() and .Add() to populate.
func Rows(cols ...string) *MockRows {
	return &MockRows{cols: cols}
}

// Add appends a data row. Must have the same length as cols.
func (r *MockRows) Add(vals ...any) *MockRows {
	r.data = append(r.data, vals)
	return r
}

// WithError sets the error to return from Err() after iteration.
func (r *MockRows) WithError(err error) *MockRows { r.err = err; return r }

// Next advances to the next row. Returns false when no more rows exist.
func (r *MockRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

// Scan copies the current row's values into the destination pointers.
func (r *MockRows) Scan(dest ...any) error {
	if r.pos == 0 || r.pos > len(r.data) {
		return fmt.Errorf("mockhost: Scan called before Next or after rows exhausted")
	}
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i < len(row) {
			switch dp := d.(type) {
			case *int:
				if v, ok := row[i].(int); ok {
					*dp = v
				}
			case *string:
				if v, ok := row[i].(string); ok {
					*dp = v
				}
			case *bool:
				if v, ok := row[i].(bool); ok {
					*dp = v
				}
			case *int64:
				if v, ok := row[i].(int64); ok {
					*dp = v
				}
			case *float64:
				if v, ok := row[i].(float64); ok {
					*dp = v
				}
			case *time.Time:
				if v, ok := row[i].(time.Time); ok {
					*dp = v
				}
			case *uuid.UUID:
				if v, ok := row[i].(uuid.UUID); ok {
					*dp = v
				}
			case core.UUIDScanner:
				v := row[i]
				if u, ok := v.(uuid.UUID); ok {
					v = u.String()
				}
				if err := dp.Scan(v); err != nil {
					return err
				}
			case core.NullUUIDScanner:
				v := row[i]
				if u, ok := v.(uuid.UUID); ok {
					v = u.String()
				}
				if err := dp.Scan(v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Close marks the row set as closed.
func (r *MockRows) Close() { r.closed = true }

// Err returns the error configured via WithError, or nil.
func (r *MockRows) Err() error {
	if r.err != nil {
		return r.err
	}
	return nil
}

// MockResult

// ExecResult creates a core.CommandTag with the given rows-affected count.
func ExecResult(n int64) core.CommandTag {
	return core.CommandTag{RowsAffected: n}
}

// MockConfig

// MockConfig implements core.Config with a simple string map.
type MockConfig struct {
	mu    sync.RWMutex
	vals  map[string]string
	lists map[string][]string
}

// NewMockConfig creates a MockConfig with empty defaults.
func NewMockConfig() *MockConfig {
	return &MockConfig{
		// A plugin that mails a link to the console refuses to start in
		// production without its URL, and the mock is production unless
		// APP_ENV says otherwise, so the mock models an install that set it.
		// Set it empty to model one that did not.
		vals:  map[string]string{core.ConfigKeyConsoleURL: TestConsoleURL},
		lists: map[string][]string{},
	}
}

// TestConsoleURL is the console URL a new MockConfig answers. It matches the
// one plugintest's database hosts answer.
const TestConsoleURL = "https://console.example.test"

// Set stores a string value for key.
func (c *MockConfig) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vals[key] = value
}

// SetStrings stores a []string value for key.
func (c *MockConfig) SetStrings(key string, values []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists[key] = values
}

// String implements core.Config.
func (c *MockConfig) String(key string) string {
	c.mu.RLock()
	v, ok := c.vals[key]
	c.mu.RUnlock()
	if ok {
		return v
	}
	return pluginKeyFromEnv(key)
}

// pluginKeyFromEnv answers a key the mock has not been given, the way the
// engine's own host does: an unknown plugin key resolves through the layered
// configuration resolver, whose highest layer is the process environment.
// Without it a plugin's own settings would be invisible under test. A value
// set on the mock still wins, so a test that wants a key to read empty sets it
// empty.
func pluginKeyFromEnv(key string) string {
	if key == "" {
		return ""
	}
	return os.Getenv(strings.ToUpper(key))
}

// Bool implements core.Config.
//
// is_production is derived rather than looked up, because that is what the
// engine does: the adapter answers it from cfg.IsProduction(), which lowercases
// APP_ENV and treats unset as production, so a test on the hardened path is on
// the engine's hardened path. An IS_PRODUCTION lookup would let a test set
// APP_ENV=production and still be told it was not in production.
func (c *MockConfig) Bool(key string) bool {
	if key == "is_production" {
		c.mu.RLock()
		v, ok := c.vals[key]
		c.mu.RUnlock()
		if ok {
			return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
		}
		return isProductionEnv()
	}

	c.mu.RLock()
	v, ok := c.vals[key]
	c.mu.RUnlock()
	if !ok {
		v = pluginKeyFromEnv(key)
	}
	return v == "true" || v == "1"
}

// Duration implements core.Config. Returns 0 for unknown keys.
func (c *MockConfig) Duration(key string) time.Duration {
	c.mu.RLock()
	v, ok := c.vals[key]
	c.mu.RUnlock()
	if !ok {
		v = pluginKeyFromEnv(key)
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

// Strings implements core.Config.
func (c *MockConfig) Strings(key string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.lists[key]; ok {
		out := make([]string, len(v))
		copy(out, v)
		return out
	}
	// Fallback: split String value on comma.
	raw, ok := c.vals[key]
	if !ok {
		raw = pluginKeyFromEnv(key)
	}
	if raw != "" {
		return splitCSV(raw)
	}
	return nil
}

// Secret implements core.SecretsProvider on MockConfig. Returns the value
// stored via Set(); returns ("", false) when the key is unset or empty.
func (c *MockConfig) Secret(key string) (val string, found bool) {
	c.mu.RLock()
	v, ok := c.vals[key]
	c.mu.RUnlock()
	if !ok {
		v = pluginKeyFromEnv(key)
	}
	if v == "" {
		return "", false
	}
	return v, true
}

// Secrets implements core.SecretsProvider on MockConfig. Returns the slice
// stored via SetStrings() or the comma-split String value.
func (c *MockConfig) Secrets(key string) (vals []string, found bool) {
	v := c.Strings(key)
	if len(v) == 0 {
		return nil, false
	}
	return v, true
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			p := s[start:i]
			if p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	return out
}

// hookSpy

// hookSpy records Subscribe and Publish calls. Plugins use this to verify
// they register hooks correctly, and to simulate hook-triggered behavior.
type hookSpy struct {
	mu            sync.RWMutex
	Events        []core.Event
	Subscriptions []struct {
		Schema  string
		Event   core.EventType
		Handler core.EventHandler
	}
	PublishErr error
}

// Subscribe records the subscription.
func (h *hookSpy) Subscribe(schema string, event core.EventType, handler core.EventHandler) core.Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Subscriptions = append(h.Subscriptions, struct {
		Schema  string
		Event   core.EventType
		Handler core.EventHandler
	}{Schema: schema, Event: event, Handler: handler})
	return &mockSubscription{}
}

// On records the system event subscription.
func (h *hookSpy) On(event string, handler core.SystemEventHandler) core.Subscription {
	return &mockSubscription{}
}

// Publish records the event and invokes matching subscribers.
// The spy always invokes matching subscribers (schema+eventType match)
// so fixture-loaded tests can simulate event propagation.
func (h *hookSpy) Publish(ctx context.Context, event core.Event) error {
	h.mu.Lock()
	var matching []core.EventHandler
	for _, s := range h.Subscriptions {
		if (s.Schema == "*" || s.Schema == event.Schema) && s.Event == event.Type {
			matching = append(matching, s.Handler)
		}
	}
	h.Events = append(h.Events, event)
	err := h.PublishErr
	h.mu.Unlock()

	if err != nil {
		return err
	}
	for _, handler := range matching {
		if hErr := handler(ctx, event); hErr != nil {
			slog.Error("hook handler error", "err", hErr, "event", event.Type, "schema", event.Schema)
		}
	}
	return nil
}

// AssertSubscribed fails if no subscription for (event, schema) was made.
func (h *hookSpy) AssertSubscribed(t T, event core.EventType, schema string) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.Subscriptions {
		if s.Event == event && s.Schema == schema {
			return
		}
	}
	t.Errorf("expected subscription %s on schema %q, but none was made", event, schema)
}

// AssertPublished fails if no event of the given (type, schema) pair was published.
func (h *hookSpy) AssertPublished(t T, eventType core.EventType, schema string) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, e := range h.Events {
		if e.Type == eventType && e.Schema == schema {
			return
		}
	}
	t.Errorf("expected event %s on schema %q to be published, but it was not", eventType, schema)
}

type mockSubscription struct{}

// Unsubscribe is a no-op for the mock subscription.
func (m *mockSubscription) Unsubscribe() {}

// mockHostAdapter

// mockHostAdapter implements core.Host backed by MockHost.
type mockHostAdapter struct {
	m *MockHost
}

// Querier returns a mock Querier backed by the spy.
func (a *mockHostAdapter) Querier(ctx context.Context) core.Querier {
	return &mockQuerier{spy: a.m.spy}
}

// QuerierRO returns the same mock Querier as Querier.
func (a *mockHostAdapter) QuerierRO(ctx context.Context) core.Querier {
	return &mockQuerier{spy: a.m.spy}
}

// Config returns the mock config.
func (a *mockHostAdapter) Config() core.Config { return a.m.conf }

// Hooks returns the hook spy.
func (a *mockHostAdapter) Hooks() core.HookBus { return a.m.hooks }

// HookPublisher returns the hook publisher backed by the hook spy.
func (a *mockHostAdapter) HookPublisher() core.HookPublisher { return a.m.hooks }

// Version returns the mock version string "mock".
func (a *mockHostAdapter) Version() string { return "mock" }

// Dialect returns "postgres" as the mock dialect.
func (a *mockHostAdapter) Dialect() string { return "postgres" }

// RawDB returns nil: no real database in mock mode.
func (a *mockHostAdapter) RawDB() *sql.DB { return nil }

// MigrationDB returns nil: no real database in mock mode.
func (a *mockHostAdapter) MigrationDB() *sql.DB { return nil }

// Schema returns nil: no real schema engine in mock mode.
func (a *mockHostAdapter) Schema() core.SchemaEngine { return nil }

// Tracer returns a no-op tracer.
func (a *mockHostAdapter) Tracer(name string) trace.Tracer { return &noop.Tracer{} }

// Secret implements core.SecretsProvider by reading directly from the
// underlying MockConfig. In tests, set encryption keys via mock.Config.Set().
func (a *mockHostAdapter) Secret(key string) (val string, found bool) {
	return a.m.conf.Secret(key)
}

// Secrets implements core.SecretsProvider. Supports "jwt_secrets".
func (a *mockHostAdapter) Secrets(key string) (vals []string, found bool) {
	return a.m.conf.Secrets(key)
}

// Logger returns a logger that writes warnings to stderr.
func (a *mockHostAdapter) Logger(ctx context.Context) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (a *mockHostAdapter) WorkerPool() *engine.WorkerPool               { return nil }
func (a *mockHostAdapter) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (a *mockHostAdapter) ParallelEngine() *engine.ParallelEngine       { return nil }
func (a *mockHostAdapter) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (a *mockHostAdapter) DistLock(name string) *engine.DistLock        { return nil }

// Capabilities grants every compiled plugin's name and every name a compiled
// plugin declares through core.FeatureDeclarer, which is what a build that
// links no licensing implementation grants. A gate on a name its plugin
// neither owns nor declares reads as unlicensed here, as it would in that
// build.
func (a *mockHostAdapter) Capabilities() core.CapabilitySet {
	names := core.CompiledFeatureNames()
	feats := make(map[string]bool, len(names))
	for _, name := range names {
		feats[name] = true
	}
	return core.CapabilitySet{
		Features: feats,
		Plan:     "free",
		State:    "free",
	}
}

func (a *mockHostAdapter) HasFeature(ctx context.Context, feature string) bool {
	if a.m.hasFeatureFunc != nil {
		return a.m.hasFeatureFunc(ctx, feature)
	}
	// Stay consistent with Capabilities, which grants every compiled name.
	// Otherwise a gate passes while HasFeature denies the same one.
	return a.Capabilities().Features[feature]
}

// mockQuerier

type mockQuerier struct {
	spy *QuerierSpy
}

// QueryRow records the call and returns a Row from the first matching expectation.
func (q *mockQuerier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	q.spy.mu.Lock()
	q.spy.Calls = append(q.spy.Calls, CallRecord{Method: "QueryRow", SQL: sql, Args: args})
	for _, e := range q.spy.ExpectRow {
		if e.SQL != sql || e.Called {
			continue
		}
		e.Called = true
		q.spy.mu.Unlock()
		row := Row().WithScan(e.ScanVals...)
		if e.ScanErr != nil {
			row.WithError(e.ScanErr)
		}
		return row, nil
	}
	q.spy.mu.Unlock()
	return Row().WithScan(), nil
}

// Query records the call and returns rows from the first matching expectation.
func (q *mockQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	q.spy.mu.Lock()
	q.spy.Calls = append(q.spy.Calls, CallRecord{Method: "Query", SQL: sql, Args: args})
	for _, e := range q.spy.ExpectQ {
		if e.SQL != sql || e.Called {
			continue
		}
		e.Called = true
		q.spy.mu.Unlock()
		if e.Err != nil {
			return nil, e.Err
		}
		if e.Rows != nil {
			return e.Rows, nil
		}
		return Rows(), nil
	}
	q.spy.mu.Unlock()
	return Rows(), nil
}

// Exec records the call and returns the result from the first matching expectation.
func (q *mockQuerier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	q.spy.mu.Lock()
	q.spy.Calls = append(q.spy.Calls, CallRecord{Method: "Exec", SQL: sql, Args: args})
	for _, e := range q.spy.ExpectE {
		if e.SQL == sql && !e.Called {
			e.Called = true
			q.spy.mu.Unlock()
			return e.Tag, e.Err
		}
	}
	q.spy.mu.Unlock()
	return core.CommandTag{}, nil
}

// Begin records the call and returns a mock transaction.
func (q *mockQuerier) Begin(ctx context.Context) (core.Tx, error) {
	q.spy.mu.Lock()
	q.spy.Calls = append(q.spy.Calls, CallRecord{Method: "Begin", SQL: "BEGIN"})
	q.spy.mu.Unlock()
	return &mockTx{spy: q.spy}, nil
}

// mockTx

type mockTx struct {
	spy *QuerierSpy
}

// QueryRow records the call and returns an empty Row.
func (t *mockTx) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	t.spy.mu.Lock()
	t.spy.Calls = append(t.spy.Calls, CallRecord{Method: "QueryRow", SQL: sql, Args: args})
	t.spy.mu.Unlock()
	return Row(), nil
}

// Query records the call and returns empty rows.
func (t *mockTx) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	t.spy.mu.Lock()
	t.spy.Calls = append(t.spy.Calls, CallRecord{Method: "Query", SQL: sql, Args: args})
	t.spy.mu.Unlock()
	return Rows(), nil
}

// Exec records the call and returns a zero CommandTag.
func (t *mockTx) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	t.spy.mu.Lock()
	t.spy.Calls = append(t.spy.Calls, CallRecord{Method: "Exec", SQL: sql, Args: args})
	t.spy.mu.Unlock()
	return core.CommandTag{}, nil
}

// Begin returns itself, supporting nested-transaction simulation.
func (t *mockTx) Begin(ctx context.Context) (core.Tx, error) { return t, nil }

// Commit is a no-op for the mock transaction.
func (t *mockTx) Commit(ctx context.Context) error { return nil }

// Rollback is a no-op for the mock transaction.
func (t *mockTx) Rollback(ctx context.Context) error { return nil }
