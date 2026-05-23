package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/engine"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// Capability.Has

func TestCapability_Has(t *testing.T) {
	tests := []struct {
		name string
		caps Capability
		need Capability
		want bool
	}{
		{"exact match", CapDBRead, CapDBRead, true},
		{"DBWrite has DBWrite", CapDBWrite, CapDBWrite, true},
		{"DBWrite implies DBRead", CapDBWrite, CapDBRead, true},
		{"CapAll has DBRead", CapAll, CapDBRead, true},
		{"CapAll has DBWrite", CapAll, CapDBWrite, true},
		{"CapAll has CapRawDB", CapAll, CapRawDB, true},
		{"CapAll has CapConfigSecret", CapAll, CapConfigSecret, true},
		{"CapAll has CapHooks", CapAll, CapHooks, true},
		{"CapAll has CapRoutes", CapAll, CapRoutes, true},
		{"CapAll has CapAdmin", CapAll, CapAdmin, true},
		{"DBRead does not have DBWrite", CapDBRead, CapDBWrite, false},
		{"DBRead does not have CapRawDB", CapDBRead, CapRawDB, false},
		{"empty caps has nothing", 0, CapDBRead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.caps.Has(tt.need)
			if got != tt.want {
				t.Errorf("Capability(%d).Has(%d) = %v, want %v", tt.caps, tt.need, got, tt.want)
			}
		})
	}
}

// Test stubs

type stubHost struct {
	callLog []string
	cfg     Config
}

func (h *stubHost) record(method string) {
	h.callLog = append(h.callLog, method)
}

func (h *stubHost) Querier(ctx context.Context) Querier {
	h.record("Querier")
	return &stubQuerier{}
}
func (h *stubHost) QuerierRO(ctx context.Context) Querier {
	h.record("QuerierRO")
	return &stubQuerier{}
}
func (h *stubHost) Logger(ctx context.Context) *slog.Logger { return slog.Default() }
func (h *stubHost) Config() Config                          { return h.cfg }
func (h *stubHost) Hooks() HookBus {
	h.record("Hooks")
	return &stubHookBus{}
}
func (h *stubHost) HookPublisher() HookPublisher {
	h.record("HookPublisher")
	return &stubHookPublisher{}
}
func (h *stubHost) Version() string      { return "v1.0.0" }
func (h *stubHost) Dialect() string      { return "postgres" }
func (h *stubHost) RawDB() *sql.DB       { return nil }
func (h *stubHost) MigrationDB() *sql.DB { return nil }
func (h *stubHost) Schema() SchemaEngine { return nil }
func (h *stubHost) Tracer(name string) trace.Tracer {
	return trace.NewNoopTracerProvider().Tracer(name)
}
func (h *stubHost) WorkerPool() *engine.WorkerPool               { return nil }
func (h *stubHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (h *stubHost) ParallelEngine() *engine.ParallelEngine       { return nil }
func (h *stubHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (h *stubHost) DistLock(name string) *engine.DistLock        { return nil }
func (h *stubHost) Capabilities() CapabilitySet {
	return CapabilitySet{Features: map[string]bool{}, Plan: "free", State: "free"}
}
func (h *stubHost) HasFeature(ctx context.Context, feature string) bool { return false }

type stubConfig struct{ vals map[string]string }

func (c *stubConfig) String(key string) string { return c.vals[key] }
func (c *stubConfig) Bool(key string) bool     { return c.vals[key] == "true" }
func (c *stubConfig) Duration(key string) time.Duration {
	d, _ := time.ParseDuration(c.vals[key])
	return d
}
func (c *stubConfig) Strings(key string) []string {
	if v := c.vals[key]; v != "" {
		return []string{v}
	}
	return nil
}

type stubQuerier struct{}

func (stubQuerier) QueryRow(ctx context.Context, sQL string, args ...any) (Row, error) {
	return nil, nil
}
func (stubQuerier) Query(ctx context.Context, sQL string, args ...any) (Rows, error) {
	return nil, nil
}
func (stubQuerier) Exec(ctx context.Context, sQL string, args ...any) (CommandTag, error) {
	return CommandTag{}, nil
}
func (stubQuerier) Begin(ctx context.Context) (Tx, error) { return nil, nil }

type stubHookBus struct{}

func (stubHookBus) Subscribe(s string, e EventType, h EventHandler) Subscription {
	return &stubSubscription{}
}

func (stubHookBus) On(event string, handler SystemEventHandler) Subscription {
	return &stubSubscription{}
}

type stubSubscription struct{}

func (stubSubscription) Unsubscribe() {}

type stubHookPublisher struct{}

func (stubHookPublisher) Publish(ctx context.Context, e Event) error { return nil }

// ScopedHost: grant/deny

func TestScopedHost_Querier_Granted(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapDBRead)
	_ = h.Querier(context.Background())
	assertCall(t, inner.callLog, "Querier")
}

func TestScopedHost_Querier_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", 0)
	q := h.Querier(context.Background())
	_, ok := q.(deniedQuerier)
	if !ok {
		t.Error("expected deniedQuerier when CapDBRead not granted")
	}
}

func TestScopedHost_QuerierRO_Granted(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapDBRead)
	_ = h.QuerierRO(context.Background())
	assertCall(t, inner.callLog, "QuerierRO")
}

func TestScopedHost_QuerierRO_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", 0)
	q := h.QuerierRO(context.Background())
	_, ok := q.(deniedQuerier)
	if !ok {
		t.Error("expected deniedQuerier when CapDBRead not granted")
	}
}

func TestScopedHost_Querier_GrantedByDBWrite(t *testing.T) {
	// CapDBWrite implies CapDBRead
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapDBWrite)
	_ = h.Querier(context.Background())
	assertCall(t, inner.callLog, "Querier")
}

func TestScopedHost_RawDB_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapDBRead)
	db := h.RawDB()
	if db != nil {
		t.Error("expected nil when CapRawDB not granted")
	}
}

func TestScopedHost_MigrationDB_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapDBRead)
	db := h.MigrationDB()
	if db != nil {
		t.Error("expected nil when CapRawDB not granted")
	}
}

func TestScopedHost_Hooks_Granted(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapHooks)
	_ = h.Hooks()
	assertCall(t, inner.callLog, "Hooks")
}

func TestScopedHost_Hooks_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", 0)
	hb := h.Hooks()
	_, ok := hb.(deniedHookBus)
	if !ok {
		t.Error("expected deniedHookBus when CapHooks not granted")
	}
}

func TestScopedHost_HookPublisher_Denied(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", 0)
	hp := h.HookPublisher()
	_, ok := hp.(deniedHookPublisher)
	if !ok {
		t.Error("expected deniedHookPublisher when CapHooks not granted")
	}
}

func TestScopedHost_Config_WithoutSecret(t *testing.T) {
	inner := &stubHost{cfg: &stubConfig{vals: map[string]string{
		"database_url": "secret-url",
		"base_url":     "https://example.com",
	}}}
	h := NewScopedHost(inner, "test", CapDBRead)
	cfg := h.Config()

	if cfg.String("database_url") != "" {
		t.Error("database_url should be empty without CapConfigSecret")
	}
	if cfg.String("base_url") != "https://example.com" {
		t.Error("base_url should pass through")
	}
}

func TestScopedHost_Config_WithSecret(t *testing.T) {
	inner := &stubHost{cfg: &stubConfig{vals: map[string]string{
		"jwt_secret": "super-secret-key",
	}}}
	h := NewScopedHost(inner, "test", CapConfigSecret)
	cfg := h.Config()

	if cfg.String("jwt_secret") != "super-secret-key" {
		t.Error("jwt_secret should be readable with CapConfigSecret")
	}
}

func TestScopedHost_Version_AlwaysAllowed(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "test", 0)
	if h.Version() != "v1.0.0" {
		t.Error("Version should always delegate")
	}
}

func TestScopedHost_Dialect_AlwaysAllowed(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "test", 0)
	if h.Dialect() != "postgres" {
		t.Error("Dialect should always delegate")
	}
}

func TestScopedHost_CapAll_NeverDenies(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "test", CapAll)
	_ = h.Querier(context.Background())
	_ = h.QuerierRO(context.Background())
	_ = h.Hooks()
	_ = h.HookPublisher()
	assertCall(t, inner.callLog, "Querier")
	assertCall(t, inner.callLog, "QuerierRO")
	assertCall(t, inner.callLog, "Hooks")
	assertCall(t, inner.callLog, "HookPublisher")
}

// ScopedConfig

func TestScopedConfig_SecretKeys_Filtered(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"jwt_secret":      "my-secret",
		"encryption_key":  "enc-key",
		"base_url":        "https://example.com",
		"api_listen_addr": ":3002",
	}}
	c := &ScopedConfig{inner: inner}

	if got := c.String("jwt_secret"); got != "" {
		t.Errorf("jwt_secret should be empty, got %q", got)
	}
	if got := c.String("encryption_key"); got != "" {
		t.Errorf("encryption_key should be empty, got %q", got)
	}
	if got := c.String("base_url"); got != "https://example.com" {
		t.Errorf("base_url should pass through, got %q", got)
	}
	if got := c.String("api_listen_addr"); got != ":3002" {
		t.Errorf("api_listen_addr should pass through, got %q", got)
	}
}

func TestScopedConfig_Strings_SecretKeys_Nil(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"jwt_secrets":  "key1,key2",
		"cors_origins": "https://example.com",
	}}
	c := &ScopedConfig{inner: inner}

	if got := c.Strings("jwt_secrets"); got != nil {
		t.Errorf("jwt_secrets Strings should be nil, got %v", got)
	}
	if got := c.Strings("cors_origins"); got == nil {
		t.Error("cors_origins Strings should not be nil")
	}
}

// An unknown config key resolves from the environment under its upper-cased
// name, so a plugin can spell the same variable either way. The upper-case
// spelling of a credential is withheld from a plugin holding no
// CapConfigSecret, exactly as the lower-case one is.
func TestScopedConfig_UpperCaseSecretKeyIsStillSecret(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"JWT_SECRET":     "my-secret",
		"NATS_TOKEN":     "broker-token",
		"ENCRYPTION_KEY": "enc-key",
		"APP_ENV":        "production",
	}}
	c := &ScopedConfig{inner: inner}

	for _, key := range []string{"JWT_SECRET", "NATS_TOKEN", "ENCRYPTION_KEY"} {
		if got := c.String(key); got != "" {
			t.Errorf("%s must be withheld from a plugin without CapConfigSecret, got %q", key, got)
		}
	}
	if got := c.String("APP_ENV"); got != "production" {
		t.Errorf("APP_ENV is not credential material and must pass through, got %q", got)
	}
}

// Strings applies the suffix rule as String does, so a plugin's own
// multi-valued credential, a key the SecretKeys map never lists, is withheld
// from Strings too.
func TestScopedConfig_Strings_AppliesTheSuffixRule(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"example_webhook_secret": "webhook-secret",
		"NATS_TOKEN":             "broker-token",
		"cors_origins":           "https://example.com",
	}}
	c := &ScopedConfig{inner: inner}

	for _, key := range []string{"example_webhook_secret", "NATS_TOKEN"} {
		if got := c.Strings(key); got != nil {
			t.Errorf("%s must be withheld from a plugin without CapConfigSecret, got %v", key, got)
		}
	}
	if got := c.Strings("cors_origins"); got == nil {
		t.Error("cors_origins is not credential material and must pass through")
	}
}

func TestScopedConfig_Bool_PassesThrough(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"multi_tenant": "true",
	}}
	c := &ScopedConfig{inner: inner}

	if got := c.Bool("multi_tenant"); !got {
		t.Error("Bool should pass through")
	}
}

func TestScopedConfig_Duration_PassesThrough(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{
		"cache_ttl": "5m",
	}}
	c := &ScopedConfig{inner: inner}

	if got := c.Duration("cache_ttl"); got != 5*time.Minute {
		t.Errorf("Duration should pass through, got %v", got)
	}
}

func TestScopedConfig_UnknownKey_EmptyString(t *testing.T) {
	inner := &stubConfig{vals: map[string]string{}}
	c := &ScopedConfig{inner: inner}

	if got := c.String("nonexistent"); got != "" {
		t.Errorf("unknown key should return empty, got %q", got)
	}
}

// Denied stubs

func TestDeniedQuerier_ReturnsErrCapDenied(t *testing.T) {
	dq := deniedQuerier{}
	_, err := dq.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrCapDenied) {
		t.Errorf("Query should return ErrCapDenied, got %v", err)
	}
	_, err = dq.Exec(context.Background(), "UPDATE t SET x=1")
	if !errors.Is(err, ErrCapDenied) {
		t.Errorf("Exec should return ErrCapDenied, got %v", err)
	}
	_, err = dq.Begin(context.Background())
	if !errors.Is(err, ErrCapDenied) {
		t.Errorf("Begin should return ErrCapDenied, got %v", err)
	}
}

func TestDeniedQuerier_QueryRow_Scan_ReturnsErrCapDenied(t *testing.T) {
	dq := deniedQuerier{}
	row, _ := dq.QueryRow(context.Background(), "SELECT 1")
	err := row.Scan()
	if !errors.Is(err, ErrCapDenied) {
		t.Errorf("Scan should return ErrCapDenied, got %v", err)
	}
}

func TestDeniedHookPublisher_Publish_ReturnsErrCapDenied(t *testing.T) {
	dhp := deniedHookPublisher{}
	err := dhp.Publish(context.Background(), Event{})
	if !errors.Is(err, ErrCapDenied) {
		t.Errorf("Publish should return ErrCapDenied, got %v", err)
	}
}

// Helpers

func assertCall(t *testing.T, log []string, method string) {
	t.Helper()
	for _, call := range log {
		if call == method {
			return
		}
	}
	t.Errorf("expected call to %s not found in log: %v", method, log)
}

// Enforcement tests

// TestCapDBWrite_ReadOnlyEnforced verifies a plugin with CapDBRead
// (but not CapDBWrite) can Query/QueryRow but Exec and Begin return
// ErrCapDenied. A plugin with CapDBWrite has full read-write access.
func TestCapDBWrite_ReadOnlyEnforced(t *testing.T) {
	inner := &stubHost{}
	ctx := context.Background()

	t.Run("CapDBRead_only_reads_work_writes_denied", func(t *testing.T) {
		h := NewScopedHost(inner, "ro-plugin", CapDBRead)
		q := h.Querier(ctx)
		_, _ = // Reads work (QueryRow through stub succeeds).
			q.QueryRow(ctx, "SELECT 1")
		_, err := q.Query(ctx, "SELECT 1")
		if err != nil {
			t.Fatal("Query should succeed with CapDBRead")
		}
		// Writes denied.
		_, err = q.Exec(ctx, "DELETE FROM t")
		if !errors.Is(err, ErrCapDenied) {
			t.Errorf("Exec should be denied, got %v", err)
		}
		_, err = q.Begin(ctx)
		if !errors.Is(err, ErrCapDenied) {
			t.Errorf("Begin should be denied, got %v", err)
		}
	})

	t.Run("CapDBWrite_read_and_write", func(t *testing.T) {
		h := NewScopedHost(inner, "rw-plugin", CapDBRead|CapDBWrite)
		q := h.Querier(ctx)
		// Reads work.
		_, err := q.Query(ctx, "SELECT 1")
		if err != nil {
			t.Fatal("Query should succeed with CapDBWrite")
		}
		// Writes work (delegate logs it).
		_, err = q.Exec(ctx, "UPDATE t SET x=1")
		if err != nil {
			t.Fatal("Exec should succeed with CapDBWrite")
		}
		assertCall(t, inner.callLog, "Querier")
	})

	t.Run("no_caps_all_denied", func(t *testing.T) {
		h := NewScopedHost(inner, "no-plugin", 0)
		q := h.Querier(ctx)
		_, err := q.Query(ctx, "SELECT 1")
		if !errors.Is(err, ErrCapDenied) {
			t.Errorf("Query should be denied with no caps, got %v", err)
		}
	})
}

// TestCapSchema_DeniedOnMutate verifies a plugin without CapSchema
// can call read ops (List, Get, PreviewDDL, ValidateContent) but
// mutating ops (Apply, Delete, ApplyPending) return ErrCapDenied.
func TestCapSchema_DeniedOnMutate(t *testing.T) {
	schema := &stubSchemaEngine{}
	inner := &schemaHost{schema: schema}
	ctx := context.Background()

	t.Run("no_CapSchema_mutate_denied", func(t *testing.T) {
		h := NewScopedHost(inner, "readonly-schema", CapDBRead)
		se := h.Schema()
		if err := se.Apply(ctx, "test", nil); !errors.Is(err, ErrCapDenied) {
			t.Errorf("Apply should be denied, got %v", err)
		}
		if err := se.Delete(ctx, "test"); !errors.Is(err, ErrCapDenied) {
			t.Errorf("Delete should be denied, got %v", err)
		}
		n, err := se.ApplyPending(ctx)
		if !errors.Is(err, ErrCapDenied) || n != 0 {
			t.Errorf("ApplyPending should be denied, got (%d, %v)", n, err)
		}
	})

	t.Run("no_CapSchema_reads_pass_through", func(t *testing.T) {
		h := NewScopedHost(inner, "readonly-schema", CapDBRead)
		se := h.Schema()
		list, err := se.List(ctx)
		if err != nil {
			t.Errorf("List should succeed, got %v", err)
		}
		if len(list) != 1 {
			t.Errorf("expected 1 item from List, got %d", len(list))
		}
	})

	t.Run("CapSchema_allows_mutate", func(t *testing.T) {
		h := NewScopedHost(inner, "rw-schema", CapSchema)
		se := h.Schema()
		if err := se.Apply(ctx, "test", nil); err != nil {
			t.Errorf("Apply should succeed with CapSchema, got %v", err)
		}
	})
}

// TestSecretKeys_Canonical verifies the SecretKeys map in core
// contains all expected entries and matches the pattern used by
// ScopedConfig and enginehost's configAdapter. api_key_pepper and
// license_cache_dir are named so the list cannot drift.
func TestSecretKeys_Canonical(t *testing.T) {
	required := []string{
		"database_url", "database_replica_url", "jwt_secret",
		"jwt_secrets", "encryption_key", "api_key_pepper",
		"redis_url", "storage_s3_key", "storage_s3_secret",
		"search_es_api_key", "search_es_password", "search_meili_api_key",
		"smtp_pass", "smtp_user", "license_key", "license_cache_dir",
		"rate_limit_redis_url", "admin_console_key_previous",
	}
	for _, key := range required {
		if !SecretKeys[key] {
			t.Errorf("SecretKeys missing %q", key)
		}
	}
	if len(SecretKeys) != len(required) {
		t.Errorf("SecretKeys has %d entries, expected %d - extra or missing keys",
			len(SecretKeys), len(required))
	}
}

// TestCapAll_ProviderForwarding verifies a CapAll plugin sees all
// provider interfaces through ScopedHost (providers are forwarded).
func TestCapAll_ProviderForwarding(t *testing.T) {
	inner := &providerHost{}
	h := NewScopedHost(inner, "full-plugin", CapAll)
	ctx := context.Background()

	// Base Host methods work.
	if h.Version() != "v1.0.0" {
		t.Error("Version should pass through")
	}
	if h.Dialect() != "postgres" {
		t.Error("Dialect should pass through")
	}

	// SessionTokenSigner.
	if _, err := h.SignSessionToken(ctx, uuid.UUID{}, "test@test", nil); err != nil {
		t.Error("SignSessionToken should pass through")
	}

	// QueryCacheProvider.
	if _, err := h.CachedFetch(ctx, "p", "t", "pg", "SELECT 1", nil, 0, nil, nil); err != nil {
		t.Error("CachedFetch should pass through")
	}
	if n := h.InvalidateCache("p:"); n != 5 {
		t.Errorf("InvalidateCache got %d, want 5", n)
	}

	// StorageConnectedProvider.
	if c := h.StorageConnected(); c == nil {
		t.Error("StorageConnected should be non-nil")
	}

	// AdminQuerierProvider.
	_ = h.AdminQuerier(ctx) // won't be denied

	// EmailSenderProvider.
	if s := h.EmailSender(); s == nil {
		t.Error("EmailSender should be non-nil")
	}

	// RefreshTokenRevoker.
	if err := h.RevokeAllRefreshTokens(ctx, "user1"); err != nil {
		t.Errorf("RevokeAllRefreshTokens got %v", err)
	}

	// TenancyConnProvider.
	_, clean, err := h.AcquireTenantConn(ctx, "t1")
	if err != nil {
		t.Errorf("AcquireTenantConn got %v", err)
	}
	clean()

	// UserProvisionerProvider.
	if p := h.UserProvisioner(); p == nil {
		t.Error("UserProvisioner should be non-nil")
	}

	// SecretsProvider.
	if _, ok := h.Secret("jwt_secret"); !ok {
		t.Error("Secret should return ok with CapAll")
	}
	if _, ok := h.Secrets("jwt_secrets"); !ok {
		t.Error("Secrets should return ok with CapAll")
	}
}

// Test stubs for provider forwarding

// schemaHost implements Host with just enough for Schema() to
// return a stub engine for CapSchema testing.
type schemaHost struct {
	stubHost
	schema SchemaEngine
}

func (h *schemaHost) Schema() SchemaEngine { return h.schema }

type stubSchemaEngine struct {
	SchemaEngine
}

func (s *stubSchemaEngine) Apply(ctx context.Context, name string, definition json.RawMessage) error {
	return nil
}
func (s *stubSchemaEngine) List(ctx context.Context) ([]json.RawMessage, error) {
	return []json.RawMessage{json.RawMessage(`{"name":"test"}`)}, nil
}

// providerHost implements enough optional provider interfaces to
// verify ScopedHost forwarding. All methods are stubs that return
// deterministic values.
type providerHost struct {
	stubHost
	storageSet         bool
	userProvisionerSet bool
}

func (p *providerHost) Storage() Storage                     { return &stubStorage{} }
func (p *providerHost) SetStorage(s Storage)                 { p.storageSet = true }
func (p *providerHost) SetUserProvisioner(s UserProvisioner) { p.userProvisionerSet = true }

func (p *providerHost) SignSessionToken(ctx context.Context, userID uuid.UUID, email string, roles []string) (string, error) {
	return "mock-token", nil
}
func (p *providerHost) CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn CacheFetcher) (bool, error) {
	return true, nil
}
func (p *providerHost) InvalidateCache(prefix string) int { return 5 }
func (p *providerHost) StorageConnected() any             { return &struct{}{} }
func (p *providerHost) AdminQuerier(ctx context.Context) Querier {
	return &stubQuerier{}
}
func (p *providerHost) EmailSender() EmailSender          { return &stubEmailSender{} }
func (p *providerHost) RegisterEmailSender(s EmailSender) {}
func (p *providerHost) RevokeAllRefreshTokens(ctx context.Context, userID string) error {
	return nil
}
func (p *providerHost) AcquireTenantConn(ctx context.Context, tenantID string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func (p *providerHost) UserProvisioner() UserProvisioner {
	return &stubUserProvisioner{}
}
func (p *providerHost) Secret(key string) (string, bool)    { return "secret", true }
func (p *providerHost) Secrets(key string) ([]string, bool) { return []string{"s1"}, true }

type stubEmailSender struct{}

func (s *stubEmailSender) Send(ctx context.Context, to []string, subject, body string) error {
	return nil
}

type stubUserProvisioner struct{}

func (s *stubUserProvisioner) FindOrCreateUser(ctx context.Context, samlProviderID uuid.UUID, email string, roles []string) (uuid.UUID, string, []string, error) {
	return uuid.Nil, "", nil, nil
}

type stubStorage struct{}

func (s *stubStorage) Put(ctx context.Context, key, contentType string, r io.Reader) (*StorageObject, error) {
	return &StorageObject{Key: key}, nil
}
func (s *stubStorage) Get(ctx context.Context, key string) (io.ReadCloser, *StorageObject, error) {
	return io.NopCloser(nil), &StorageObject{Key: key}, nil
}
func (s *stubStorage) Delete(ctx context.Context, key string) error { return nil }
func (s *stubStorage) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "https://signed.example.com/" + key, nil
}
func (s *stubStorage) List(ctx context.Context, prefix string, limit int) ([]*StorageObject, error) {
	return nil, nil
}

// ScopedHost: Storage / SetStorage / SetUserProvisioner forwarding

// TestScopedHost_ProviderStorageForwarding verifies Storage, SetStorage,
// and SetUserProvisioner forward through ScopedHost when inner implements
// the corresponding provider interfaces.
func TestScopedHost_ProviderStorageForwarding(t *testing.T) {
	inner := &providerHost{}
	h := NewScopedHost(inner, "storage-plugin", CapAll)

	t.Run("Storage_forwarded", func(t *testing.T) {
		s := h.Storage()
		if s == nil {
			t.Error("Storage should be non-nil when inner implements StorageProvider")
		}
	})

	t.Run("SetStorage_forwarded", func(t *testing.T) {
		h.SetStorage(&stubStorage{})
		if !inner.storageSet {
			t.Error("SetStorage should forward to inner StorageSetter")
		}
	})

	t.Run("SetUserProvisioner_forwarded", func(t *testing.T) {
		h.SetUserProvisioner(&stubUserProvisioner{})
		if !inner.userProvisionerSet {
			t.Error("SetUserProvisioner should forward to inner UserProvisionerSetter")
		}
	})
}

// TestScopedHost_ProviderStorageFallback verifies Storage returns nil
// and SetStorage/SetUserProvisioner are no-ops when inner does NOT
// implement the provider interfaces.
func TestScopedHost_ProviderStorageFallback(t *testing.T) {
	inner := &stubHost{}
	h := NewScopedHost(inner, "no-storage-plugin", 0)

	t.Run("Storage_nil_when_not_implemented", func(t *testing.T) {
		s := h.Storage()
		if s != nil {
			t.Error("Storage should be nil when inner does not implement StorageProvider")
		}
	})

	t.Run("SetStorage_noop_when_not_implemented", func(t *testing.T) {
		// Should not panic.
		h.SetStorage(&stubStorage{})
	})

	t.Run("SetUserProvisioner_noop_when_not_implemented", func(t *testing.T) {
		// Should not panic.
		h.SetUserProvisioner(&stubUserProvisioner{})
	})
}

// Both console keys verify a signature that names any client address, so
// both are secret. The suffix rule catches the current one only.
func TestIsSecretKey_BothConsoleKeys(t *testing.T) {
	for _, key := range []string{"ADMIN_CONSOLE_KEY", "ADMIN_CONSOLE_KEY_PREVIOUS", "admin_console_key_previous"} {
		if !IsSecretKey(key) {
			t.Errorf("IsSecretKey(%q) = false, want true", key)
		}
	}
}

type regionResolverHost struct {
	stubHost
	resolver TenantRegionResolver
}

func (h *regionResolverHost) TenantRegionResolver() TenantRegionResolver { return h.resolver }

type staticRegion struct{}

func (staticRegion) TenantRegion(context.Context, string) (string, bool, error) {
	return "eu-west-1", true, nil
}

// The region role needs no capability, so a plugin granted nothing still
// reads it, and an inner host that offers none answers nil for the reader to
// fall back on.
func TestScopedHost_TenantRegionResolver_ForwardsWithoutACapability(t *testing.T) {
	h := NewScopedHost(&regionResolverHost{resolver: staticRegion{}}, "reader", 0)
	r := h.TenantRegionResolver()
	if r == nil {
		t.Fatal("TenantRegionResolver() = nil, want the inner host's resolver")
	}
	if region, assigned, _ := r.TenantRegion(context.Background(), "acme"); region != "eu-west-1" || !assigned {
		t.Errorf("TenantRegion() = %q, %v, want eu-west-1, true", region, assigned)
	}

	if got := NewScopedHost(&stubHost{}, "reader", CapAll).TenantRegionResolver(); got != nil {
		t.Errorf("TenantRegionResolver() on a host with no role = %v, want nil", got)
	}
}
