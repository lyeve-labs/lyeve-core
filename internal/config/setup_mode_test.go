package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

func setupModeEnv(t *testing.T, mode, dbURL, jwt, enc string) {
	t.Helper()
	t.Setenv("LYEVE_SETUP_MODE", mode)
	t.Setenv("DATABASE_URL", dbURL)
	t.Setenv("JWT_SECRET", jwt)
	t.Setenv("ENCRYPTION_KEY", enc)
}

func missingKeys(err *config.SetupRequiredError) []string {
	out := make([]string, len(err.Missing))
	for i, s := range err.Missing {
		out[i] = s.EnvKey
	}
	return out
}

func TestLoad_SetupMode(t *testing.T) {
	const db = "postgres://localhost/cms"
	const jwt = "test-secret-16+chars!"
	const enc = "separate-encryption-key-128-bits-long"

	t.Run("off and no database fails the boot", func(t *testing.T) {
		setupModeEnv(t, "", "", jwt, enc)
		_, err := config.Load()
		require.Error(t, err)
		_, setup := config.IsSetupRequired(err)
		assert.False(t, setup)
		assert.Contains(t, err.Error(), "DATABASE_URL must be set")
	})
	t.Run("false is off", func(t *testing.T) {
		setupModeEnv(t, "false", "", jwt, enc)
		_, err := config.Load()
		_, setup := config.IsSetupRequired(err)
		assert.False(t, setup)
		require.Error(t, err)
	})
	t.Run("on with no database asks for it", func(t *testing.T) {
		setupModeEnv(t, "true", "", jwt, enc)
		_, err := config.Load()
		req, setup := config.IsSetupRequired(err)
		require.True(t, setup, "err = %v", err)
		assert.Equal(t, []string{"DATABASE_URL"}, missingKeys(req))
		assert.Equal(t, "database.url", req.Missing[0].YAMLPath)
		assert.False(t, req.Missing[0].Secret)
	})
	t.Run("on with no secrets asks for both", func(t *testing.T) {
		setupModeEnv(t, "true", db, "", "")
		_, err := config.Load()
		req, setup := config.IsSetupRequired(err)
		require.True(t, setup, "err = %v", err)
		assert.Equal(t, []string{"JWT_SECRET", "ENCRYPTION_KEY"}, missingKeys(req))
		for _, s := range req.Missing {
			assert.True(t, s.Secret, s.EnvKey)
		}
	})
	t.Run("on with only the encryption key missing asks for it", func(t *testing.T) {
		setupModeEnv(t, "true", db, jwt, "")
		_, err := config.Load()
		req, setup := config.IsSetupRequired(err)
		require.True(t, setup, "err = %v", err)
		assert.Equal(t, []string{"ENCRYPTION_KEY"}, missingKeys(req))
	})
	t.Run("on with nothing missing boots normally", func(t *testing.T) {
		setupModeEnv(t, "true", db, jwt, enc)
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, db, cfg.DatabaseURL)
	})
	t.Run("carries the operator token and listen addresses", func(t *testing.T) {
		setupModeEnv(t, "1", "", "", "")
		t.Setenv("LYEVE_SETUP_TOKEN", "operator-setup-token-0123")
		t.Setenv("ADMIN_LISTEN_ADDR", "127.0.0.1:4999")
		_, err := config.Load()
		req, setup := config.IsSetupRequired(err)
		require.True(t, setup, "err = %v", err)
		assert.Equal(t, "operator-setup-token-0123", req.SetupToken)
		assert.Equal(t, "127.0.0.1:4999", req.AdminListenAddr)
		assert.Equal(t, "0.0.0.0:3002", req.APIListenAddr)
	})
	t.Run("a short operator token is refused", func(t *testing.T) {
		setupModeEnv(t, "true", "", "", "")
		t.Setenv("LYEVE_SETUP_TOKEN", "short")
		_, err := config.Load()
		require.Error(t, err)
		_, setup := config.IsSetupRequired(err)
		assert.False(t, setup)
		assert.Contains(t, err.Error(), "LYEVE_SETUP_TOKEN")
	})
	t.Run("an unparseable value is refused", func(t *testing.T) {
		setupModeEnv(t, "allow", "", jwt, enc)
		_, err := config.Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LYEVE_SETUP_MODE")
	})
}
