package httpx

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// testKeyStore wraps *encryption.KeyStore to implement security.KeyStore.
type testKeyStore struct {
	inner *encryption.KeyStore
}

func (k *testKeyStore) Encrypt(ctx context.Context, tenantID string, plaintext []byte) (string, error) {
	if len(plaintext) == 0 {
		return "", nil
	}
	return k.inner.EncryptPlaintext(tenantID, plaintext)
}

func (k *testKeyStore) Decrypt(ctx context.Context, tenantID string, ciphertext string) ([]byte, error) {
	if ciphertext == "" {
		return nil, nil
	}
	return k.inner.DecryptPlaintext(tenantID, ciphertext)
}

// testHost implements the minimum core.Host surface needed for testing
// EncryptPluginSecret / DecryptPluginSecret.
type testHost struct {
	keyStore security.KeyStore
	cfg      testConfig
}

func (h *testHost) KeyStore() security.KeyStore                  { return h.keyStore }
func (h *testHost) Config() core.Config                          { return &h.cfg }
func (h *testHost) Querier(ctx context.Context) core.Querier     { return nil }
func (h *testHost) QuerierRO(ctx context.Context) core.Querier   { return nil }
func (h *testHost) Logger(ctx context.Context) *slog.Logger      { return slog.Default() }
func (h *testHost) Hooks() core.HookBus                          { return nil }
func (h *testHost) HookPublisher() core.HookPublisher            { return nil }
func (h *testHost) Version() string                              { return "test" }
func (h *testHost) Dialect() string                              { return "postgres" }
func (h *testHost) RawDB() *sql.DB                               { return nil }
func (h *testHost) MigrationDB() *sql.DB                         { return nil }
func (h *testHost) Schema() core.SchemaEngine                    { return nil }
func (h *testHost) Tracer(name string) trace.Tracer              { return &noop.Tracer{} }
func (h *testHost) WorkerPool() *engine.WorkerPool               { return nil }
func (h *testHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (h *testHost) ParallelEngine() *engine.ParallelEngine       { return nil }
func (h *testHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (h *testHost) DistLock(name string) *engine.DistLock        { return nil }
func (h *testHost) Capabilities() core.CapabilitySet {
	return core.CapabilitySet{Features: map[string]bool{}, Plan: "free", State: "free"}
}
func (h *testHost) HasFeature(ctx context.Context, feature string) bool { return false }

// SecretsProvider implementation: allows encKey() to find jwt_secrets.
func (h *testHost) Secret(key string) (string, bool) {
	return "", false
}

func (h *testHost) Secrets(key string) ([]string, bool) {
	if key == "jwt_secrets" {
		s := h.cfg.Strings(key)
		if len(s) > 0 {
			return s, true
		}
	}
	return nil, false
}

type testConfig struct {
	jwtSecrets []string
}

func (c *testConfig) String(key string) string          { return "" }
func (c *testConfig) Bool(key string) bool              { return false }
func (c *testConfig) Duration(key string) time.Duration { return 0 }
func (c *testConfig) Strings(key string) []string {
	if key == "jwt_secrets" {
		out := make([]string, len(c.jwtSecrets))
		copy(out, c.jwtSecrets)
		return out
	}
	return nil
}

func TestEncryptPluginSecret_EmptyPlaintext(t *testing.T) {
	host := &testHost{keyStore: nil}
	result, err := EncryptPluginSecret(host, "tenant-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestDecryptPluginSecret_EmptyCiphertext(t *testing.T) {
	host := &testHost{keyStore: nil}
	result, err := DecryptPluginSecret(host, "tenant-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestEncryptDecryptPluginSecret_WithKeyStore(t *testing.T) {
	ks, err := encryption.NewKeyStore("test-master-key-for-encryption-tests-ok")
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	host := &testHost{keyStore: &testKeyStore{inner: ks}}

	plaintext := "smtp-password-123"
	tenantID := "tenant-42"

	ciphertext, err := EncryptPluginSecret(host, tenantID, plaintext)
	if err != nil {
		t.Fatalf("EncryptPluginSecret: %v", err)
	}
	if ciphertext == "" || ciphertext == plaintext {
		t.Fatalf("ciphertext should be non-empty and not equal plaintext: got %q", ciphertext)
	}

	decrypted, err := DecryptPluginSecret(host, tenantID, ciphertext)
	if err != nil {
		t.Fatalf("DecryptPluginSecret: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("decrypted mismatch: got %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptDecryptPluginSecret_DifferentTenants(t *testing.T) {
	ks, err := encryption.NewKeyStore("master-key-tenant-isolation-test")
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	// Provide a different jwt_secret so the fallback path also fails.
	host := &testHost{
		keyStore: &testKeyStore{inner: ks},
		cfg: testConfig{
			jwtSecrets: []string{"different-legacy-secret-for-fallback-fail"},
		},
	}

	secret := "shared-secret-value"

	cipherA, err := EncryptPluginSecret(host, "tenant-a", secret)
	if err != nil {
		t.Fatalf("EncryptPluginSecret tenant-a: %v", err)
	}

	// Decrypt with wrong tenant should fail: both KeyStore (GCM auth fail)
	// and legacy fallback (wrong key) cannot decrypt.
	_, err = DecryptPluginSecret(host, "tenant-b", cipherA)
	if err == nil {
		t.Errorf("expected error when decrypting with wrong tenant, got nil")
	}

	decrypted, err := DecryptPluginSecret(host, "tenant-a", cipherA)
	if err != nil {
		t.Fatalf("DecryptPluginSecret tenant-a: %v", err)
	}
	if decrypted != secret {
		t.Errorf("decrypted mismatch: got %q, want %q", decrypted, secret)
	}
}

func TestEncryptPluginSecret_FallbackToJWTSecret(t *testing.T) {
	// No KeyStore configured: falls back to jwt_secrets[0].
	host := &testHost{
		keyStore: nil,
		cfg: testConfig{
			jwtSecrets: []string{"fallback-jwt-secret-32-bytes-min!!!", "secondary-secret"},
		},
	}

	plaintext := "legacy-secret"
	tenantID := "any-tenant" // tenant ID is ignored on fallback path

	ciphertext, err := EncryptPluginSecret(host, tenantID, plaintext)
	if err != nil {
		t.Fatalf("EncryptPluginSecret: %v", err)
	}
	if ciphertext == "" || ciphertext == plaintext {
		t.Fatalf("ciphertext should be non-empty: got %q", ciphertext)
	}

	decrypted, err := DecryptPluginSecret(host, tenantID, ciphertext)
	if err != nil {
		t.Fatalf("DecryptPluginSecret: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("decrypted mismatch: got %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptPluginSecret_CrossFormatCompatibility(t *testing.T) {
	// When both KeyStore and jwt_secrets are available, KeyStore takes
	// precedence but DecryptPluginSecret falls back to jwt_secrets[0]
	// when KeyStore decryption fails.
	ks, err := encryption.NewKeyStore("key-store-master-key-tenant-dek-ok")
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	legacySecret := "legacy-jwt-secret-32-bytes-for-testing!!!"
	host := &testHost{
		keyStore: &testKeyStore{inner: ks},
		cfg: testConfig{
			jwtSecrets: []string{legacySecret},
		},
	}

	// Encrypt with legacy path first (simulate pre-migration ciphertext).
	legacyCipherHex, err := security.EncryptSecret("pre-migration-secret", legacySecret)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	// Decrypt with full host: should fall back to jwt_secrets[0] after
	// KeyStore decryption fails (wrong tenant DEK).
	tenantID := "new-tenant"
	decrypted, err := DecryptPluginSecret(host, tenantID, legacyCipherHex)
	if err != nil {
		t.Fatalf("DecryptPluginSecret cross-format: %v", err)
	}
	if decrypted != "pre-migration-secret" {
		t.Errorf("decrypted mismatch: got %q, want %q", decrypted, "pre-migration-secret")
	}
}

func TestEncryptPluginSecret_NoEncryptionConfigured(t *testing.T) {
	host := &testHost{keyStore: nil, cfg: testConfig{jwtSecrets: nil}}

	result, err := EncryptPluginSecret(host, "tenant-1", "plain-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "plain-secret" {
		t.Errorf("expected plaintext passthrough, got %q", result)
	}

	decrypted, err := DecryptPluginSecret(host, "tenant-1", "plain-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decrypted != "plain-secret" {
		t.Errorf("expected plaintext passthrough, got %q", decrypted)
	}
}

func TestEncryptPluginSecret_FallbackEncryptNewFormat(t *testing.T) {
	host := &testHost{
		keyStore: nil,
		cfg: testConfig{
			jwtSecrets: []string{"my-fallback-jwt-secret-for-encrypt-test"},
		},
	}

	plaintext := stringWithLength("A", 100)
	tenantID := "tenant-xyz"

	ciphertext, err := EncryptPluginSecret(host, tenantID, plaintext)
	if err != nil {
		t.Fatalf("EncryptPluginSecret: %v", err)
	}
	if ciphertext == "" {
		t.Fatal("ciphertext should be non-empty")
	}

	decrypted, err := DecryptPluginSecret(host, tenantID, ciphertext)
	if err != nil {
		t.Fatalf("DecryptPluginSecret: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("decrypted mismatch: got %q, want %q", decrypted, plaintext)
	}

	// PBKDF2 ciphertext includes a salt prefix (16 bytes = 32 hex chars)
	if len(ciphertext) <= 56 {
		t.Errorf("PBKDF2 ciphertext too short (%d hex chars), likely old format", len(ciphertext))
	}
}

func stringWithLength(base string, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(base)
	}
	return sb.String()
}
