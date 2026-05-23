package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// statelessEnv clears every setting a stateless boot reads or refuses, so a
// value from the developer's shell cannot decide the case.
func statelessEnv(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("LYEVE_CONFIG", "")
	t.Setenv("LYEVE_MODE", mode)
	for _, k := range []string{"DATABASE_URL", "DATABASE_REPLICA_URL", "JWT_SECRET", "ENCRYPTION_KEY", "LYEVE_SETUP_MODE", "MULTI_TENANT"} {
		t.Setenv(k, "")
	}
	t.Setenv("APP_ENV", "development")
}

func TestLoad_StatelessModeBootsWithNoDatabaseAndNoSecrets(t *testing.T) {
	statelessEnv(t, "stateless")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Stateless())
	assert.Empty(t, cfg.DatabaseURL)
	assert.Empty(t, cfg.JWTSecret)
	assert.Empty(t, cfg.EncryptionKey, "no fallback to a JWT secret that does not exist")
}

func TestLoad_StatelessModeIsCaseInsensitive(t *testing.T) {
	statelessEnv(t, "Stateless")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.Stateless())
}

func TestLoad_WithoutTheModeADatabaseIsStillRequired(t *testing.T) {
	statelessEnv(t, "")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL must be set")
}

func TestLoad_AnUnknownModeStopsTheBoot(t *testing.T) {
	// A typo must not boot the database engine the operator meant to avoid.
	statelessEnv(t, "statless")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `LYEVE_MODE must be "stateless" or unset`)
}

func TestLoad_StatelessModeRefusesWhatItCannotHonor(t *testing.T) {
	cases := []struct {
		key, value, want string
	}{
		{"DATABASE_URL", "postgres://app@db/app", "DATABASE_URL is set"},
		{"DATABASE_REPLICA_URL", "postgres://app@replica/app", "DATABASE_REPLICA_URL is set"},
		{"LYEVE_SETUP_MODE", "true", "LYEVE_SETUP_MODE"},
		{"MULTI_TENANT", "true", "MULTI_TENANT"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			statelessEnv(t, "stateless")
			t.Setenv(tc.key, tc.value)

			_, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "LYEVE_MODE=stateless")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoad_StatelessProductionChecksOnlyWhatItServes(t *testing.T) {
	statelessEnv(t, "stateless")
	t.Setenv("APP_ENV", "production")
	t.Setenv("RATE_LIMIT_RPS", "")
	t.Setenv("SECURE_COOKIE", "")

	_, err := config.Load()
	require.Error(t, err, "an API open to the network still needs the per-IP limiter")
	assert.Contains(t, err.Error(), "RATE_LIMIT_RPS")
	assert.Contains(t, err.Error(), "SECURE_COOKIE", "HSTS and the HTTPS redirect still protect the keys")
	for _, absent := range []string{"JWT_SECRET", "ENCRYPTION_KEY", "LYEVE_AUDIT_HMAC_KEY"} {
		assert.NotContains(t, err.Error(), absent, "a stateless engine has no %s to protect", absent)
	}

	t.Setenv("RATE_LIMIT_RPS", "100")
	t.Setenv("SECURE_COOKIE", "true")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.ProductionWarnings(), "no database, so no warning about its TLS")
}
