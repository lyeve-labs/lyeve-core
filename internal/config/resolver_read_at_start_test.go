package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

type rowsStore []db.PluginConfigRow

func (s rowsStore) ListAll(context.Context) ([]db.PluginConfigRow, error) { return s, nil }

func storeSettings(t *testing.T, values map[string]any) {
	t.Helper()
	_, err := RefreshAdminLayer(context.Background(), rowsStore{{PluginName: CoreSettingsOwner, Config: values}})
	require.NoError(t, err)
}

// Load reads the token lifetime before the stored layer is installed. A value
// saved for it stays editable and is reported as waiting for the next start.
func TestResolver_KeyReadOnlyAtStartIsFlaggedAndStaysEditable(t *testing.T) {
	r := NewResolver(nil)
	prev := SetActiveResolver(r)
	t.Cleanup(func() { SetActiveResolver(prev) })

	r.Get("JWT_EXPIRY_SECS")
	storeSettings(t, map[string]any{"JWT_EXPIRY_SECS": "1800"})

	res := r.Resolve("JWT_EXPIRY_SECS")
	assert.True(t, res.Overridable)
	assert.True(t, res.ReadAtStart)
	assert.Equal(t, SourceAdmin, res.From)
	assert.Equal(t, "1800", res.Value)
}

func TestResolver_KeyReadAgainAfterTheStoredLayerIsNotFlagged(t *testing.T) {
	r := NewResolver(nil)
	prev := SetActiveResolver(r)
	t.Cleanup(func() { SetActiveResolver(prev) })

	r.Get("EXAMPLE_HOST")
	storeSettings(t, map[string]any{"EXAMPLE_HOST": "mail.example"})

	assert.Equal(t, "mail.example", r.Get("EXAMPLE_HOST"))
	assert.False(t, r.Resolve("EXAMPLE_HOST").ReadAtStart)
}

// bootConfig loads the engine's settings from the environment alone, the way
// boot does before the database is open.
func bootConfig(t *testing.T) *Config {
	t.Helper()
	for k, v := range map[string]string{
		"APP_ENV":        "development",
		"DATABASE_URL":   "postgres://boot@localhost/boot",
		"JWT_SECRET":     "stored-settings-jwt-secret-0123456789abcdef",
		"ENCRYPTION_KEY": "stored-settings-encryption-key-0123456789ab",
		"LYEVE_CONFIG":   t.TempDir(),
	} {
		t.Setenv(k, v)
	}
	prev := ActiveResolver()
	t.Cleanup(func() { SetActiveResolver(prev) })
	cfg, err := Load()
	require.NoError(t, err)
	return cfg
}

func TestApplyStoredSettings_StoredValueTakesEffectAtStart(t *testing.T) {
	cfg := bootConfig(t)
	require.Equal(t, int64(defaultJWTExpirySecs), cfg.JWTExpirySecs)

	storeSettings(t, map[string]any{
		"JWT_EXPIRY_SECS":          "1800",
		"DATABASE_MAX_CONNECTIONS": "3",
	})
	changed, err := ApplyStoredSettings(cfg)
	require.NoError(t, err)

	assert.Equal(t, int64(1800), cfg.JWTExpirySecs)
	assert.Equal(t, int32(defaultMaxConns), cfg.DatabaseMaxConns, "the pool was opened before the stored layer, so its size keeps the boot value")
	assert.Equal(t, []string{"JWTExpirySecs"}, changed)
	assert.True(t, ActiveResolver().Resolve("JWT_EXPIRY_SECS").ReadAtStart,
		"the second read is part of starting, so a later save still waits for the next start")
}

func TestApplyStoredSettings_EnvironmentStillWins(t *testing.T) {
	t.Setenv("JWT_EXPIRY_SECS", "600")
	cfg := bootConfig(t)

	storeSettings(t, map[string]any{"JWT_EXPIRY_SECS": "1800"})
	changed, err := ApplyStoredSettings(cfg)
	require.NoError(t, err)

	assert.Equal(t, int64(600), cfg.JWTExpirySecs)
	assert.Empty(t, changed)
}

func TestApplyStoredSettings_InvalidStoredValueKeepsTheBootValues(t *testing.T) {
	cfg := bootConfig(t)

	storeSettings(t, map[string]any{"JWT_EXPIRY_SECS": "a week"})
	changed, err := ApplyStoredSettings(cfg)
	require.Error(t, err)

	assert.Equal(t, int64(defaultJWTExpirySecs), cfg.JWTExpirySecs)
	assert.Empty(t, changed)
}
