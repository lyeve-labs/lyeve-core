package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// A denied capability hands the plugin a stub whose return value is
// indistinguishable from the real thing. These tests pin the log line that is
// the only evidence the call did nothing.

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// capturingHost is a stubHost whose Logger writes structured records into buf.
type capturingHost struct {
	*stubHost
	buf *lockedBuffer
}

func newCapturingHost() *capturingHost {
	return &capturingHost{stubHost: &stubHost{}, buf: &lockedBuffer{}}
}

func (h *capturingHost) Logger(ctx context.Context) *slog.Logger {
	return slog.New(slog.NewJSONHandler(h.buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type logRecord map[string]any

// denialRecords returns every "plugin capability denied" record the host logged.
func denialRecords(t *testing.T, h *capturingHost) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range bytes.Split([]byte(h.buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %s (%v)", line, err)
		}
		if rec["msg"] == "plugin capability denied" {
			out = append(out, rec)
		}
	}
	return out
}

func requireOneDenial(t *testing.T, h *capturingHost) logRecord {
	t.Helper()
	recs := denialRecords(t, h)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 denial log record, got %d: %q", len(recs), h.buf.String())
	}
	return recs[0]
}

func assertField(t *testing.T, rec logRecord, key, want string) {
	t.Helper()
	if got, _ := rec[key].(string); got != want {
		t.Errorf("denial log field %q = %q, want %q", key, got, want)
	}
}

func TestScopedHost_DeniedHookSubscribe_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "graphql", 0)

	sub := sh.Hooks().Subscribe("article", AfterCreate, func(context.Context, Event) error { return nil })
	if sub == nil {
		t.Fatal("Subscribe must still return a Subscription")
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "plugin", "graphql")
	assertField(t, rec, "op", "Hooks.Subscribe")
	assertField(t, rec, "capability", "CapHooks")
	assertField(t, rec, "target", "article:after_create")
	if rec["level"] != "WARN" {
		t.Errorf("denial must log at WARN, got %v", rec["level"])
	}
}

func TestScopedHost_DeniedSystemEventSubscribe_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "graphql", 0)

	sh.Hooks().On("plugin.reloaded", func(context.Context, HookEvent) error { return nil })

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "Hooks.On")
	assertField(t, rec, "capability", "CapHooks")
	assertField(t, rec, "target", "plugin.reloaded")
}

// A gated plugin rebuilds its handlers from this event and the engine
// re-collects routes straight after, expecting the rebuild to have happened,
// so the event reaches a plugin that holds no CapHooks.
func TestScopedHost_LicenseChangedReachesAPluginWithoutHooks(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "oauth", 0)

	sub := sh.Hooks().On(EventLicenseChanged, func(context.Context, HookEvent) error { return nil })

	if recs := denialRecords(t, h); len(recs) != 0 {
		t.Fatalf("license notification logged %d denial(s), want 0", len(recs))
	}
	if _, denied := sub.(deniedSubscription); denied {
		t.Fatal("subscription is the denied stub; it never reached the real bus")
	}
}

func TestScopedHost_DeniedPublish_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "content", 0)

	if err := sh.HookPublisher().Publish(context.Background(), Event{Type: AfterCreate}); err == nil {
		t.Error("denied Publish must still return an error")
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "HookPublisher.Publish")
	assertField(t, rec, "capability", "CapHooks")
}

// A plugin that follows the canonical Start() pattern nil-checks MigrationDB
// and skips its migrations, so the denial surfaces much later as a missing
// table. The log line is what ties the two together.
func TestScopedHost_DeniedMigrationDB_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "widgets", CapDBRead)

	if sh.MigrationDB() != nil {
		t.Fatal("MigrationDB must stay nil without CapRawDB")
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "MigrationDB")
	assertField(t, rec, "capability", "CapRawDB")
}

func TestScopedHost_DeniedRawDB_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "widgets", CapDBRead)

	if sh.RawDB() != nil {
		t.Fatal("RawDB must stay nil without CapRawDB")
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "RawDB")
	assertField(t, rec, "capability", "CapRawDB")
}

// EngineDBConn hands out a connection on the engine's own database, carrying
// no DML guard and no tenant scoping. That is the reach CapRawDB buys, so it
// costs the same capability as RawDB and MigrationDB.
func TestScopedHost_DeniedEngineDBConn_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "widgets", CapDBRead)

	conn, err := sh.EngineDBConn(context.Background())
	if conn != nil {
		t.Fatal("EngineDBConn must hand out no connection without CapRawDB")
	}
	if !errors.Is(err, ErrCapDenied) {
		t.Fatalf("err = %v, want ErrCapDenied", err)
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "EngineDBConn")
	assertField(t, rec, "capability", "CapRawDB")
}

func TestScopedHost_DeniedSecretConfig_IsLogged(t *testing.T) {
	h := newCapturingHost()
	h.cfg = &stubConfig{vals: map[string]string{"smtp_pass": "hunter2"}}
	sh := NewScopedHost(h, "mailer", CapDBRead)

	if got := sh.Config().String("smtp_pass"); got != "" {
		t.Fatalf("secret must read empty without CapConfigSecret, got %q", got)
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "Config.String")
	assertField(t, rec, "capability", "CapConfigSecret")
	assertField(t, rec, "target", "smtp_pass")
}

// QueryRow returns a nil error, so the denial would otherwise only appear when
// the caller scans.
func TestScopedHost_DeniedQueryRow_IsLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "analytics", 0)

	row, err := sh.Querier(context.Background()).QueryRow(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("QueryRow error contract changed: %v", err)
	}
	if row == nil {
		t.Fatal("QueryRow must still return a Row")
	}

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "Querier.QueryRow")
	assertField(t, rec, "capability", "CapDBRead")
}

// A denial on a per-request path must not flood the log, but a second distinct
// target must still be reported.
func TestScopedHost_DenialLog_OncePerTarget(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "graphql", 0)

	handler := func(context.Context, Event) error { return nil }
	for range 5 {
		sh.Hooks().Subscribe("article", AfterCreate, handler)
	}
	if got := len(denialRecords(t, h)); got != 1 {
		t.Fatalf("repeat subscription to one topic logged %d times, want 1", got)
	}

	sh.Hooks().Subscribe("page", AfterUpdate, handler)
	if got := len(denialRecords(t, h)); got != 2 {
		t.Fatalf("second topic logged %d records in total, want 2", got)
	}
}

func TestScopedHost_GrantedCapability_LogsNothing(t *testing.T) {
	h := newCapturingHost()
	h.cfg = &stubConfig{vals: map[string]string{"smtp_pass": "hunter2"}}
	sh := NewScopedHost(h, "webhook", CapAll)

	sh.Hooks().Subscribe("article", AfterCreate, func(context.Context, Event) error { return nil })
	sh.Hooks().On("license.changed", func(context.Context, HookEvent) error { return nil })
	_ = sh.HookPublisher().Publish(context.Background(), Event{Type: AfterCreate})
	_ = sh.MigrationDB()
	_ = sh.RawDB()
	_ = sh.Config().String("smtp_pass")

	if recs := denialRecords(t, h); len(recs) != 0 {
		t.Fatalf("granted capabilities logged %d denials: %q", len(recs), h.buf.String())
	}
}

// A plugin cannot see the unexported denied stubs from its own repo, and
// Capabilities() reports license features rather than plugin capabilities.
// CapabilityChecker is the supported way to refuse to start.
func TestScopedHost_HasCapability_ReportsDenial(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "graphql", CapDBRead|CapRoutes)

	var host Host = sh
	cc, ok := host.(CapabilityChecker)
	if !ok {
		t.Fatal("ScopedHost must satisfy CapabilityChecker through the Host interface")
	}
	if cc.HasCapability(CapHooks) {
		t.Error("HasCapability(CapHooks) must be false when the grant is absent")
	}
	if !cc.HasCapability(CapDBRead) {
		t.Error("HasCapability(CapDBRead) must be true when the grant is held")
	}
	if !cc.HasCapability(CapRoutes) {
		t.Error("HasCapability(CapRoutes) must be true when the grant is held")
	}
}
