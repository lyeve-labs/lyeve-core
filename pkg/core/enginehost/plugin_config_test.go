package enginehost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TestConfig_UnknownPluginKeyResolvesFromEnv pins the route for keys core
// does not own. configAdapter answers a closed switch of the keys core owns,
// and any other key resolves from the environment under its upper-cased name
// rather than reading as though the operator had chosen "" or false or 0.
func TestConfig_UnknownPluginKeyResolvesFromEnv(t *testing.T) {
	c := &configAdapter{cfg: &config.Config{}}

	if got := c.String("example_keyspace"); got != "" {
		t.Fatalf("unset key = %q; want empty", got)
	}

	t.Setenv("EXAMPLE_KEYSPACE", "tenant-scoped")
	if got := c.String("example_keyspace"); got != "tenant-scoped" {
		t.Errorf("String = %q; want the configured value", got)
	}

	t.Setenv("EXAMPLE_DISABLED", "true")
	if !c.Bool("example_disabled") {
		t.Error("Bool did not resolve a plugin key from the environment")
	}

	t.Setenv("EXAMPLE_INTERVAL", "45m")
	if got := c.Duration("example_interval"); got.Minutes() != 45 {
		t.Errorf("Duration = %v; want 45m", got)
	}
}

// TestConfig_UnparseableValuesReadAsZero guards the fallback from turning an
// operator typo into a boot failure: an unparseable value reads as the zero
// value, as an unset key does.
func TestConfig_UnparseableValuesReadAsZero(t *testing.T) {
	c := &configAdapter{cfg: &config.Config{}}

	t.Setenv("EXAMPLE_DISABLED", "yes-please")
	if c.Bool("example_disabled") {
		t.Error("unparseable bool must stay false")
	}

	// A bare integer has no unit, so it is rejected rather than guessed at.
	t.Setenv("EXAMPLE_INTERVAL", "45")
	if got := c.Duration("example_interval"); got != 0 {
		t.Errorf("unitless duration = %v; want 0", got)
	}
}

// TestConfig_PluginCredentialsStayRedacted is the security half of the route.
// core.SecretKeys can only name keys core itself configures, so a plugin
// credential core has never heard of would otherwise reach the unredacted
// Config channel through the environment fallback. The suffix rule closes
// that path.
func TestConfig_PluginCredentialsStayRedacted(t *testing.T) {
	c := &configAdapter{cfg: &config.Config{}}

	for _, key := range []string{
		"example_webhook_secret",
		"some_plugin_secret",
		"some_plugin_password",
		"some_plugin_token",
	} {
		t.Setenv(upper(key), "leaked")
		if got := c.String(key); got != "" {
			t.Errorf("Config.String(%q) = %q; credential material must never come through Config", key, got)
		}
		if !core.IsSecretKey(key) {
			t.Errorf("IsSecretKey(%q) = false; the suffix rule missed a credential", key)
		}
	}

	// The rule must not over-redact. A KMS key id names which key to use and
	// is not key material, so a plugin has to be able to read it.
	t.Setenv("KMS_KEY_ID", "arn:aws:kms:eu-west-1:111122223333:key/abcd")
	if got := c.String("kms_key_id"); got == "" {
		t.Error("kms_key_id was redacted; it is an identifier, not a credential")
	}
}

// TestSecret_UnknownPluginCredentialResolvesFromEnv is the other side: what
// Config refuses to serve, SecretsProvider must, or a plugin credential core
// never heard of has no channel at all.
func TestSecret_UnknownPluginCredentialResolvesFromEnv(t *testing.T) {
	h := &engineHost{cfg: &config.Config{}}

	if _, found := h.Secret("example_webhook_secret"); found {
		t.Fatal("unset credential reported as found")
	}

	t.Setenv("EXAMPLE_WEBHOOK_SECRET", "webhook-secret")
	val, found := h.Secret("example_webhook_secret")
	if !found || val != "webhook-secret" {
		t.Errorf("Secret = (%q, %v); want the configured value", val, found)
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - 32
		}
	}
	return string(out)
}

// The overlay carries operator-set configuration from sys_plugin_config. The
// environment wins: a variable set in the deployment must hold against a value
// saved in the admin UI.
func TestPluginEnv_OverlayFillsUnsetKeysOnly(t *testing.T) {
	t.Setenv("OVERLAY_FROM_ENV", "env-value")
	SetPluginConfigOverlay(map[string]string{
		"overlay_from_env":    "stored-value",
		"overlay_stored_only": "stored-value",
	})
	t.Cleanup(func() { SetPluginConfigOverlay(nil) })

	if got := pluginEnv("overlay_from_env"); got != "env-value" {
		t.Errorf("env-set key = %q, want the environment to win", got)
	}
	if got := pluginEnv("overlay_stored_only"); got != "stored-value" {
		t.Errorf("stored-only key = %q, want %q", got, "stored-value")
	}
	if got := pluginEnv("overlay_absent"); got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
}

// An empty environment variable is a deliberate choice ("unset this"), so the
// overlay must not treat it as absent and paper over it.
func TestPluginEnv_EmptyEnvVarBeatsOverlay(t *testing.T) {
	t.Setenv("OVERLAY_EMPTY", "")
	SetPluginConfigOverlay(map[string]string{"overlay_empty": "stored-value"})
	t.Cleanup(func() { SetPluginConfigOverlay(nil) })

	if got := pluginEnv("overlay_empty"); got != "" {
		t.Errorf("explicitly empty variable = %q, want empty", got)
	}
}

// A duration key core does not own resolves through the layered resolver, so
// the value an operator sets is the value the plugin reads, never a constant
// of the engine's.
func TestPluginEnvDuration_ReadsAPluginDurationKey(t *testing.T) {
	t.Setenv("EXAMPLE_LOCK_TTL", "45s")
	t.Setenv("EXAMPLE_DATA_TTL", "72h")

	assert.Equal(t, 45*time.Second, pluginEnvDuration("example_lock_ttl"))
	assert.Equal(t, 72*time.Hour, pluginEnvDuration("example_data_ttl"))
}

func TestPluginEnvDuration_UnsetIsZero(t *testing.T) {
	// Zero is what lets the plugin apply its own default.
	t.Setenv("EXAMPLE_LOCK_TTL", "")
	t.Setenv("EXAMPLE_DATA_TTL", "")

	assert.Zero(t, pluginEnvDuration("example_lock_ttl"))
	assert.Zero(t, pluginEnvDuration("example_data_ttl"))
}
