package config_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// APP_ENV defaults to production, which runs ValidateProduction and rejects the
// deliberately minimal fixtures below. These tests exercise parsing and
// defaults, not the production gate, so the whole binary runs as development.
// Tests that do care set APP_ENV themselves.
func TestMain(m *testing.M) {
	if err := os.Setenv("APP_ENV", "development"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("JWT_SECRET")

	cfg, err := config.Load()
	assert.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestLoad_MissingJWTSecret(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Unsetenv("JWT_SECRET")

	cfg, err := config.Load()
	assert.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "JWT_SECRET")
}

func TestLoad_MinimalValid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "postgres://localhost/cms", cfg.DatabaseURL)
	assert.Equal(t, "test-secret-16+chars", cfg.JWTSecret)
	// Four instances at this size fit inside stock PostgreSQL's 100 connections,
	// 3 of which are held for superusers, so one instance cannot claim the
	// server and leave a second replica unable to connect.
	assert.Equal(t, int32(25), cfg.DatabaseMaxConns)
	assert.Equal(t, int64(900), cfg.JWTExpirySecs)
	assert.Equal(t, "0.0.0.0:3001", cfg.AdminListenAddr)
	assert.Equal(t, "0.0.0.0:3002", cfg.APIListenAddr)
}

// The schema engine builds its DDL from DatabaseDriver while every other
// consumer resolves the engine from the DSN. When the two disagree the engine
// boots and migrates cleanly and then fails on the first content-type create,
// so the derived default matters more than it looks.
func TestLoad_DatabaseDriverFollowsTheDSN(t *testing.T) {
	os.Setenv("JWT_SECRET", "test-secret-16+chars")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Unsetenv("DATABASE_DRIVER")

	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"postgres scheme", "postgres://localhost/cms", "postgres"},
		{"postgresql scheme", "postgresql://localhost/cms", "postgres"},
		{"mysql scheme", "mysql://root:pw@tcp(localhost:3306)/cms", "mysql"},
		{"mysql driver form", "root:pw@tcp(localhost:3306)/cms", "mysql"},
		{"sqlserver scheme", "sqlserver://sa:pw@localhost:1433?database=cms", "mssql"},
		{"keyword-form postgres", "host=localhost dbname=cms", "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("DATABASE_URL", tc.dsn)
			defer os.Unsetenv("DATABASE_URL")

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.DatabaseDriver)
		})
	}

	t.Run("explicit DATABASE_DRIVER still wins", func(t *testing.T) {
		os.Setenv("DATABASE_URL", "mysql://root:pw@tcp(localhost:3306)/cms")
		defer os.Unsetenv("DATABASE_URL")
		os.Setenv("DATABASE_DRIVER", "postgres")
		defer os.Unsetenv("DATABASE_DRIVER")

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, "postgres", cfg.DatabaseDriver)
	})
}

func TestLoad_JWTExpirySecs(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("JWT_EXPIRY_SECS", "3600")
	defer os.Unsetenv("JWT_EXPIRY_SECS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int64(3600), cfg.JWTExpirySecs)
}

func TestLoad_JWTExpirySecs_Invalid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("JWT_EXPIRY_SECS", "abc")
	defer os.Unsetenv("JWT_EXPIRY_SECS")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_EXPIRY_SECS")
}

func TestLoad_DatabaseMaxConnections(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DATABASE_MAX_CONNECTIONS", "200")
	defer os.Unsetenv("DATABASE_MAX_CONNECTIONS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int32(200), cfg.DatabaseMaxConns)
}

func TestLoad_DatabaseMaxConnections_Invalid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DATABASE_MAX_CONNECTIONS", "NaN")
	defer os.Unsetenv("DATABASE_MAX_CONNECTIONS")

	_, err := config.Load()
	assert.Error(t, err)
}

func TestLoad_CORSOrigins(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CORS_ORIGINS", "https://example.com,https://app.com")
	defer os.Unsetenv("CORS_ORIGINS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"https://example.com", "https://app.com"}, cfg.CORSOrigins)
}

func TestLoad_CORS_Defaults(t *testing.T) {
	os.Unsetenv("CORS_ALLOW_HEADERS")
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"http://localhost:5173"}, cfg.CORSOrigins)
	assert.Equal(t, 3600, cfg.CORSPreflightMaxAge)
	assert.Contains(t, cfg.CORSAllowMethods, "PATCH")
	// The admin SPA sends the double-submit value in X-CSRF-Token. A default
	// allowlist that omits it makes the browser strip the header from every
	// cross-origin write, which the server then answers 403 "csrf token
	// required" with nothing to say why.
	assert.Contains(t, cfg.CORSAllowHeaders, "X-CSRF-Token")
}

func TestLoad_MultiTenant(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("MULTI_TENANT", "true")
	defer os.Unsetenv("MULTI_TENANT")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.MultiTenant)
}

func TestLoad_MultiTenant_DefaultFalse(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.MultiTenant)
}

func TestLoad_RateLimit(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("RATE_LIMIT_RPS", "100")
	defer os.Unsetenv("RATE_LIMIT_RPS")
	os.Setenv("RATE_LIMIT_BURST", "50")
	defer os.Unsetenv("RATE_LIMIT_BURST")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 100.0, cfg.RateLimitRPS)
	assert.Equal(t, 50, cfg.RateLimitBurst)
}

func TestLoad_DatabaseDriver(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	tests := []struct {
		name   string
		driver string
	}{
		{"postgres", "postgres"},
		{"mysql", "mysql"},
		{"mssql", "mssql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.driver != "" {
				os.Setenv("DATABASE_DRIVER", tt.driver)
				defer os.Unsetenv("DATABASE_DRIVER")
			}
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tt.driver, cfg.DatabaseDriver)
		})
	}
}

func TestLoad_DatabaseDriver_Default(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres", cfg.DatabaseDriver)
}

func TestLoad_PoolConfig(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DB_POOL_MIN_CONNS", "5")
	defer os.Unsetenv("DB_POOL_MIN_CONNS")
	os.Setenv("DB_CONN_MAX_LIFETIME", "30m")
	defer os.Unsetenv("DB_CONN_MAX_LIFETIME")
	os.Setenv("DB_CONN_MAX_IDLE_TIME", "2m")
	defer os.Unsetenv("DB_CONN_MAX_IDLE_TIME")
	os.Setenv("DB_POOL_HEALTH_CHECK_PERIOD", "15s")
	defer os.Unsetenv("DB_POOL_HEALTH_CHECK_PERIOD")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int32(5), cfg.DatabaseMinConns)
	assert.Equal(t, 30*time.Minute, cfg.DatabaseConnMaxLifetime)
	assert.Equal(t, 2*time.Minute, cfg.DatabaseConnMaxIdleTime)
	assert.Equal(t, 15*time.Second, cfg.DatabaseHealthCheckPeriod)
}

func TestLoad_PoolHealthOptions(t *testing.T) {
	os.Setenv("POOL_HEALTH_MAX_LATENCY", "500ms")
	defer os.Unsetenv("POOL_HEALTH_MAX_LATENCY")
	os.Setenv("POOL_HEALTH_MIN_IDLE", "3")
	defer os.Unsetenv("POOL_HEALTH_MIN_IDLE")
	os.Setenv("POOL_HEALTH_MAX_UTIL", "0.85")
	defer os.Unsetenv("POOL_HEALTH_MAX_UTIL")

	latency, minIdle, maxUtil := config.LoadPoolHealthOptions()
	assert.Equal(t, 500*time.Millisecond, latency)
	assert.Equal(t, 3, minIdle)
	assert.Equal(t, 0.85, maxUtil)
}

func TestLoad_PoolHealthOptions_Defaults(t *testing.T) {
	os.Unsetenv("POOL_HEALTH_MAX_LATENCY")
	os.Unsetenv("POOL_HEALTH_MIN_IDLE")
	os.Unsetenv("POOL_HEALTH_MAX_UTIL")

	latency, minIdle, maxUtil := config.LoadPoolHealthOptions()
	assert.Equal(t, 1*time.Second, latency)
	assert.Equal(t, 1, minIdle)
	assert.Equal(t, 0.9, maxUtil)
}

func TestLoad_EncryptionKey_Fallback(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "secret-key-12345678")
	defer os.Unsetenv("JWT_SECRET")
	os.Unsetenv("ENCRYPTION_KEY")

	// Load() falls back EncryptionKey to JWT_SECRET, which triggers
	// ValidateCritical: EncryptionKey == JWTSecret is a key hierarchy
	// violation and fails in all environments.
	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENCRYPTION_KEY must not equal JWT_SECRET")
}

func TestLoad_EncryptionKey_Explicit(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "jwt-secret-16+chars")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "enc-key-256bit-longer-longer-128")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "enc-key-256bit-longer-longer-128", cfg.EncryptionKey, "should use explicit ENCRYPTION_KEY")
}

func TestLoad_APIListenAddr(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("ADMIN_LISTEN_ADDR", "0.0.0.0:8080")
	defer os.Unsetenv("ADMIN_LISTEN_ADDR")
	os.Setenv("API_LISTEN_ADDR", "0.0.0.0:8081")
	defer os.Unsetenv("API_LISTEN_ADDR")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8080", cfg.AdminListenAddr)
	assert.Equal(t, "0.0.0.0:8081", cfg.APIListenAddr)
}

func TestLoad_JWTSecrets(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "primary-key-16+chars")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("JWT_SECRETS", "new-secret,old-secret")
	defer os.Unsetenv("JWT_SECRETS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"new-secret", "old-secret"}, cfg.JWTSecrets)
}

func TestLoad_SecureCookie(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("SECURE_COOKIE", "true")
	defer os.Unsetenv("SECURE_COOKIE")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.SecureCookie)
}

func TestLoad_SecureCookie_DefaultFalse(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.SecureCookie)
}

func TestLoad_InstanceID(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("INSTANCE_ID", "cms-prod-1")
	defer os.Unsetenv("INSTANCE_ID")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "cms-prod-1", cfg.InstanceID)
}

func TestLoad_JWTAlg(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("JWT_ALG", "HS256")
	defer os.Unsetenv("JWT_ALG")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "HS256", cfg.JWTAlg)
}

func TestLoad_JWTAlg_Default(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "EdDSA", cfg.JWTAlg)
}

func TestLoad_RefreshTokenTTL(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("REFRESH_TOKEN_TTL_SECS", "86400")
	defer os.Unsetenv("REFRESH_TOKEN_TTL_SECS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int64(86400), cfg.RefreshTokenTTL)
}

func TestLoad_GracefulShutdownTimeout(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("GRACEFUL_SHUTDOWN_SECS", "30")
	defer os.Unsetenv("GRACEFUL_SHUTDOWN_SECS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.GracefulShutdownTimeout)
}

func TestLoad_CacheConfig(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CACHE_TTL", "120s")
	defer os.Unsetenv("CACHE_TTL")
	os.Setenv("CACHE_MAX_ENTRIES", "500")
	defer os.Unsetenv("CACHE_MAX_ENTRIES")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 120*time.Second, cfg.CacheTTL)
	assert.Equal(t, 500, cfg.CacheMaxEntries)
}

func TestLoad_TracingSamplingRate(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("TRACING_SAMPLING_RATE", "0.5")
	defer os.Unsetenv("TRACING_SAMPLING_RATE")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 0.5, cfg.TracingSamplingRate)
}

func TestLoad_TracingSamplingRate_Invalid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("TRACING_SAMPLING_RATE", "abc")
	defer os.Unsetenv("TRACING_SAMPLING_RATE")

	_, err := config.Load()
	assert.Error(t, err)
}

func TestLoad_TracingSamplingRate_OutOfRange(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("TRACING_SAMPLING_RATE", "1.5")
	defer os.Unsetenv("TRACING_SAMPLING_RATE")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "between 0.0 and 1.0")
}

func TestLoad_PoolConfig_PgBouncer(t *testing.T) {
	os.Setenv("CONNECTION_POOLER", "pgbouncer")
	defer os.Unsetenv("CONNECTION_POOLER")
	os.Setenv("POOL_MAX_CLIENT_CONN", "200")
	defer os.Unsetenv("POOL_MAX_CLIENT_CONN")
	os.Setenv("POOL_DEFAULT_POOL_SIZE", "30")
	defer os.Unsetenv("POOL_DEFAULT_POOL_SIZE")

	cfg, mode := config.LoadPoolConfig()
	assert.Equal(t, "pgbouncer", mode)
	require.NotNil(t, cfg)
	assert.Equal(t, int32(200), cfg.MaxClientConn)
	assert.Equal(t, int32(30), cfg.DefaultPoolSize)
}

func TestLoad_PoolConfig_TenantSizes(t *testing.T) {
	os.Setenv("CONNECTION_POOLER", "pgbouncer")
	defer os.Unsetenv("CONNECTION_POOLER")
	os.Setenv("POOL_TENANT_SIZES", `[{"slug":"acme","pool_size":50,"min_pool_size":10}]`)
	defer os.Unsetenv("POOL_TENANT_SIZES")

	cfg, _ := config.LoadPoolConfig()
	require.NotNil(t, cfg)
	require.Contains(t, cfg.PerTenantPools, "acme")
	assert.Equal(t, int32(50), cfg.PerTenantPools["acme"].PoolSize)
	assert.Equal(t, int32(10), cfg.PerTenantPools["acme"].MinPoolSize)
}

func TestLoad_PoolConfig_None(t *testing.T) {
	os.Unsetenv("CONNECTION_POOLER")
	cfg, mode := config.LoadPoolConfig()
	assert.Nil(t, cfg)
	assert.Equal(t, "", mode)
}

func TestParseTenantSamplingRates(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("TRACING_TENANT_SAMPLING_RATES", "acme=0.1,corp=0.5")
	defer os.Unsetenv("TRACING_TENANT_SAMPLING_RATES")

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.TracingTenantSamplingRates)
	assert.Equal(t, 0.1, cfg.TracingTenantSamplingRates["acme"])
	assert.Equal(t, 0.5, cfg.TracingTenantSamplingRates["corp"])
}

func TestParseTenantSamplingRates_InvalidPair(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("TRACING_TENANT_SAMPLING_RATES", "acme")
	defer os.Unsetenv("TRACING_TENANT_SAMPLING_RATES")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pair")
}

func TestLoad_StorageConfig(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("STORAGE_DRIVER", "s3")
	defer os.Unsetenv("STORAGE_DRIVER")
	os.Setenv("STORAGE_S3_BUCKET", "my-bucket")
	defer os.Unsetenv("STORAGE_S3_BUCKET")
	os.Setenv("STORAGE_S3_REGION", "eu-west-1")
	defer os.Unsetenv("STORAGE_S3_REGION")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "s3", cfg.StorageDriver)
	assert.Equal(t, "my-bucket", cfg.StorageS3Bucket)
	assert.Equal(t, "eu-west-1", cfg.StorageS3Region)
}

func TestLoad_DatabaseReplica(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DATABASE_REPLICA_URL", "postgres://replica/cms")
	defer os.Unsetenv("DATABASE_REPLICA_URL")
	os.Setenv("DATABASE_REPLICA_MAX_CONNS", "50")
	defer os.Unsetenv("DATABASE_REPLICA_MAX_CONNS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://replica/cms", cfg.DatabaseReplicaURL)
	assert.Equal(t, int32(50), cfg.DatabaseReplicaMaxConns)
}

func TestLoad_InstanceRegion(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("INSTANCE_REGION", "us-east")
	defer os.Unsetenv("INSTANCE_REGION")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "us-east", cfg.InstanceRegion)
}

func TestLoad_RateLimitBackend(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("RATE_LIMIT_BACKEND", "redis")
	defer os.Unsetenv("RATE_LIMIT_BACKEND")
	os.Setenv("RATE_LIMIT_PER_TENANT", "true")
	defer os.Unsetenv("RATE_LIMIT_PER_TENANT")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "redis", cfg.RateLimitBackend)
	assert.True(t, cfg.RateLimitPerTenant)
}

// TestLoad_CacheTTL_Invalid verifies that a non-empty, unparseable
// CACHE_TTL fails with an error, never a silent fallback to the default.
func TestLoad_CacheTTL_Invalid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CACHE_TTL", "abc")
	defer os.Unsetenv("CACHE_TTL")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CACHE_TTL")
}

// TestLoad_CacheMaxEntries_InvalidNonNumeric verifies that a
// non-numeric CACHE_MAX_ENTRIES fails with an error instead of
// silently falling back to the default.
func TestLoad_CacheMaxEntries_InvalidNonNumeric(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CACHE_MAX_ENTRIES", "abc")
	defer os.Unsetenv("CACHE_MAX_ENTRIES")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CACHE_MAX_ENTRIES")
}

// TestLoad_CacheMaxEntries_NonPositive verifies that 0 or negative
// CACHE_MAX_ENTRIES fails with an error.
func TestLoad_CacheMaxEntries_NonPositive(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CACHE_MAX_ENTRIES", "0")
	defer os.Unsetenv("CACHE_MAX_ENTRIES")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CACHE_MAX_ENTRIES")
	assert.Contains(t, err.Error(), "positive")
}

// TestLoad_MalformedDotenv_Warns verifies that a malformed .env file
// (not absent: actually present but containing parse errors) produces
// a stderr warning so operators know their configuration is broken.
func TestLoad_MalformedDotenv_Warns(t *testing.T) {
	// Create a temp dir, write a malformed .env, and chdir into it
	// so godotenv.Load() picks it up.
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	// "A-B=C": hyphen is illegal in variable names, triggers godotenv parse error
	require.NoError(t, os.WriteFile(envPath, []byte("A-B=C\n"), 0644))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(dir))

	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	// Capture stderr
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w

	cfg, cfgErr := config.Load()

	// Restore stderr before assertions
	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	// config.Load should still succeed (malformed .env is non-fatal)
	require.NoError(t, cfgErr)
	require.NotNil(t, cfg)

	// stderr should contain the warning
	stderrOutput := buf.String()
	assert.Contains(t, stderrOutput, "WARNING:")
	assert.Contains(t, stderrOutput, "failed to parse")
}

// non-positive numeric guards: ensure DATABASE_MAX_CONNECTIONS <= 0
// is rejected so operators get a clear error instead of silently accepting a
// configuration that would fail at connection time.
func TestLoad_DatabaseMaxConnections_NonPositive(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("DATABASE_MAX_CONNECTIONS", "0")
	defer os.Unsetenv("DATABASE_MAX_CONNECTIONS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_MAX_CONNECTIONS")
	assert.Contains(t, err.Error(), "positive")
}

func TestLoad_DatabaseMaxConnections_Negative(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("DATABASE_MAX_CONNECTIONS", "-5")
	defer os.Unsetenv("DATABASE_MAX_CONNECTIONS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_MAX_CONNECTIONS")
	assert.Contains(t, err.Error(), "positive")
}

func TestLoad_MaxBodyBytes_NonPositive(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("MAX_BODY_BYTES", "0")
	defer os.Unsetenv("MAX_BODY_BYTES")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "MAX_BODY_BYTES")
	assert.Contains(t, err.Error(), "positive")
}

func TestLoad_DBMinConns_Negative(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("DB_POOL_MIN_CONNS", "-1")
	defer os.Unsetenv("DB_POOL_MIN_CONNS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DB_POOL_MIN_CONNS")
	assert.Contains(t, err.Error(), ">= 0")
}

func TestLoad_DBMinConns_ZeroValid(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("DB_POOL_MIN_CONNS", "0")
	defer os.Unsetenv("DB_POOL_MIN_CONNS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, int32(0), cfg.DatabaseMinConns)
}

func TestLoad_GracefulShutdownSecs_NonPositive(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("GRACEFUL_SHUTDOWN_SECS", "0")
	defer os.Unsetenv("GRACEFUL_SHUTDOWN_SECS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GRACEFUL_SHUTDOWN_SECS")
	assert.Contains(t, err.Error(), "positive")
}

func TestLoad_GracefulShutdownSecs_Negative(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("GRACEFUL_SHUTDOWN_SECS", "-5")
	defer os.Unsetenv("GRACEFUL_SHUTDOWN_SECS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "GRACEFUL_SHUTDOWN_SECS")
	assert.Contains(t, err.Error(), "positive")
}

func TestLoad_PreStopDrainDelay(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("PRESTOP_DRAIN_SECS", "10")
	defer os.Unsetenv("PRESTOP_DRAIN_SECS")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, cfg.PreStopDrainDelay)
}

func TestLoad_PreStopDrainSecs_Default(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	// PRESTOP_DRAIN_SECS not set -> default 5s.

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, cfg.PreStopDrainDelay)
}

func TestLoad_PreStopDrainSecs_Negative(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("PRESTOP_DRAIN_SECS", "-5")
	defer os.Unsetenv("PRESTOP_DRAIN_SECS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PRESTOP_DRAIN_SECS")
	assert.Contains(t, err.Error(), "non-negative")
}

func TestLoad_DatabaseReplicaMaxConns_NonPositive(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	os.Setenv("DATABASE_REPLICA_URL", "postgres://replica/cms")
	defer os.Unsetenv("DATABASE_REPLICA_URL")
	os.Setenv("DATABASE_REPLICA_MAX_CONNS", "0")
	defer os.Unsetenv("DATABASE_REPLICA_MAX_CONNS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_REPLICA_MAX_CONNS")
	assert.Contains(t, err.Error(), "positive")
}

// Validate Tests

func TestValidate_Defaults(t *testing.T) {
	// Default values (JWT_ALG=EdDSA, PASSWORD_HASH_ALGO=bcrypt) should pass.
	cfg := &config.Config{
		JWTAlg:           "EdDSA",
		PasswordHashAlgo: "bcrypt",
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_HappyPaths(t *testing.T) {
	tests := []struct {
		name         string
		jwtAlg       string
		passwordAlgo string
		wantErr      bool
	}{
		{"EdDSA+bcrypt", "EdDSA", "bcrypt", false},
		{"EdDSA+argon2id", "EdDSA", "argon2id", false},
		{"HS256+bcrypt", "HS256", "bcrypt", false},
		{"HS256+argon2id", "HS256", "argon2id", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				JWTAlg:           tt.jwtAlg,
				PasswordHashAlgo: tt.passwordAlgo,
			}
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidate_RejectsInvalidJWTAlg(t *testing.T) {
	cfg := &config.Config{
		JWTAlg:           "RS256", // unsupported
		PasswordHashAlgo: "bcrypt",
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_ALG")
	assert.Contains(t, err.Error(), "RS256")
}

func TestValidate_RejectsInvalidPasswordHashAlgo(t *testing.T) {
	cfg := &config.Config{
		JWTAlg:           "EdDSA",
		PasswordHashAlgo: "argon2d", // typo: should be argon2id
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PASSWORD_HASH_ALGO")
	assert.Contains(t, err.Error(), "argon2d")
}

func TestLoad_RejectsInvalidJWTAlg(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("JWT_ALG", "RS256")
	defer os.Unsetenv("JWT_ALG")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_ALG")
}

func TestLoad_RejectsInvalidPasswordHashAlgo(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("PASSWORD_HASH_ALGO", "scrypt")
	defer os.Unsetenv("PASSWORD_HASH_ALGO")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PASSWORD_HASH_ALGO")
}

func TestLoad_AcceptsAllValidCombos(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"default-EdDSA+bcrypt": {"JWT_ALG": "", "PASSWORD_HASH_ALGO": ""},
		"EdDSA+bcrypt":         {"JWT_ALG": "EdDSA", "PASSWORD_HASH_ALGO": "bcrypt"},
		"HS256+bcrypt":         {"JWT_ALG": "HS256", "PASSWORD_HASH_ALGO": "bcrypt"},
		"EdDSA+argon2id":       {"JWT_ALG": "EdDSA", "PASSWORD_HASH_ALGO": "argon2id"},
		"HS256+argon2id":       {"JWT_ALG": "HS256", "PASSWORD_HASH_ALGO": "argon2id"},
	} {
		t.Run(name, func(t *testing.T) {
			os.Setenv("DATABASE_URL", "postgres://localhost/cms")
			defer os.Unsetenv("DATABASE_URL")
			os.Setenv("JWT_SECRET", "test-secret-16+chars!")
			defer os.Unsetenv("JWT_SECRET")
			os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
			defer os.Unsetenv("ENCRYPTION_KEY")
			for k, v := range env {
				if v != "" {
					os.Setenv(k, v)
					defer os.Unsetenv(k)
				} else {
					os.Unsetenv(k)
				}
			}
			cfg, err := config.Load()
			assert.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

// Boot validation: Load() must fail fast on invalid drivers/providers

func TestLoad_RejectsInvalidDatabaseDriver(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DATABASE_DRIVER", "sqlite")
	defer os.Unsetenv("DATABASE_DRIVER")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_DRIVER")
	assert.Contains(t, err.Error(), "sqlite")
}

func TestLoad_RejectsInvalidStorageDriver(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("STORAGE_DRIVER", "gcs")
	defer os.Unsetenv("STORAGE_DRIVER")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "STORAGE_DRIVER")
	assert.Contains(t, err.Error(), "gcs")
}

func TestLoad_AcceptsValidDrivers(t *testing.T) {
	tests := []struct {
		name           string
		databaseDriver string
		storageDriver  string
	}{
		{"all-defaults", "", ""},
		{"postgres-local", "postgres", "local"},
		{"mysql-s3", "mysql", "s3"},
		{"mssql-empty", "mssql", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("DATABASE_URL", "postgres://localhost/cms")
			defer os.Unsetenv("DATABASE_URL")
			os.Setenv("JWT_SECRET", "test-secret-16+chars!")
			defer os.Unsetenv("JWT_SECRET")
			os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
			defer os.Unsetenv("ENCRYPTION_KEY")
			if tt.databaseDriver != "" {
				os.Setenv("DATABASE_DRIVER", tt.databaseDriver)
				defer os.Unsetenv("DATABASE_DRIVER")
			}
			if tt.storageDriver != "" {
				os.Setenv("STORAGE_DRIVER", tt.storageDriver)
				defer os.Unsetenv("STORAGE_DRIVER")
			}
			cfg, err := config.Load()
			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	}
}

// TestLoad_RejectsCORSWildcard verifies that ValidateCritical catches
// CORS wildcards at boot time (not just in production).
func TestLoad_RejectsCORSWildcard(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("CORS_ORIGINS", "*")
	defer os.Unsetenv("CORS_ORIGINS")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "CORS_ORIGINS")
	assert.Contains(t, err.Error(), "wildcard")
}

// TestLoad_RejectsShortJWTSecret verifies that ValidateCritical catches
// short JWT secrets at boot time.
func TestLoad_RejectsShortJWTSecret(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "short")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_SECRET")
	assert.Contains(t, err.Error(), "too short")
}

// TestLoad_RejectsWeakJWTSecret verifies that ValidateCritical catches
// weak JWT secrets at boot time.
func TestLoad_RejectsWeakJWTSecret(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "changeme")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_SECRET")
	assert.Contains(t, err.Error(), "too weak")
}

// TestLoad_RejectsEncryptionKeyEqualsJWTSecret verifies that ValidateCritical
// catches EncryptionKey == JWTSecret at boot time.
func TestLoad_RejectsEncryptionKeyEqualsJWTSecret(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "test-secret-16+chars!")
	defer os.Unsetenv("ENCRYPTION_KEY")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ENCRYPTION_KEY")
	assert.Contains(t, err.Error(), "must not equal JWT_SECRET")
}

// TestLoad_RejectsShortEncryptionKey verifies that ValidateCritical catches
// short encryption keys at boot time.
func TestLoad_RejectsShortEncryptionKey(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16+chars!")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "short-key")
	defer os.Unsetenv("ENCRYPTION_KEY")

	_, err := config.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ENCRYPTION_KEY")
	assert.Contains(t, err.Error(), "too short")
}

// TestLoad_RejectsMultipleValidateCriticalErrors verifies that multiple
// validation failures from ValidateCritical are reported together.
func TestLoad_RejectsMultipleValidateCriticalErrors(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "changeme")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("DATABASE_DRIVER", "sqlite")
	defer os.Unsetenv("DATABASE_DRIVER")
	os.Setenv("STORAGE_DRIVER", "gcs")
	defer os.Unsetenv("STORAGE_DRIVER")

	_, err := config.Load()
	assert.Error(t, err)
	msg := err.Error()
	// Should report both weak secret and driver violation
	assert.Contains(t, msg, "JWT_SECRET")
	assert.Contains(t, msg, "too weak")
	assert.Contains(t, msg, "DATABASE_DRIVER")
	assert.Contains(t, msg, "STORAGE_DRIVER")
}

// PluginCapsStrictMode

func TestLoad_PluginCapsStrictMode_DefaultFalse(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16-chars-min")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.PluginCapsStrictMode, "default should be false")
}

func TestLoad_PluginCapsStrictMode_True(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16-chars-min")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("PLUGIN_CAPS_STRICT_MODE", "true")
	defer os.Unsetenv("PLUGIN_CAPS_STRICT_MODE")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.PluginCapsStrictMode)
}

func TestLoad_PluginCapsStrictMode_One(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16-chars-min")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("PLUGIN_CAPS_STRICT_MODE", "1")
	defer os.Unsetenv("PLUGIN_CAPS_STRICT_MODE")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.PluginCapsStrictMode, "1 should be truthy")
}

func TestLoad_PluginCapsStrictMode_Zero(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/cms")
	defer os.Unsetenv("DATABASE_URL")
	os.Setenv("JWT_SECRET", "test-secret-16-chars-min")
	defer os.Unsetenv("JWT_SECRET")
	os.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	defer os.Unsetenv("ENCRYPTION_KEY")
	os.Setenv("PLUGIN_CAPS_STRICT_MODE", "0")
	defer os.Unsetenv("PLUGIN_CAPS_STRICT_MODE")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.PluginCapsStrictMode, "0 should be falsy")
}

// Provenance can only report a setting the environment alone supplies if the
// engine has asked for it, so the registry has to be populated by the real boot
// read rather than by a hand-kept list. A .env file becomes process
// environment, so an install configured that way must still report every
// env-pinned key.
func TestLoad_EnvOnlySettingsAreVisibleToProvenance(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "test-secret-16+chars")
	t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	t.Setenv("CORS_ORIGINS", "https://app.example.com")

	_, err := config.Load()
	require.NoError(t, err)

	byKey := map[string]config.Resolution{}
	for _, res := range config.ActiveResolver().Provenance() {
		byKey[res.Key] = res
	}

	for _, key := range []string{"CORS_ORIGINS", "DATABASE_URL", "JWT_SECRET"} {
		require.Contains(t, byKey, key, "the engine read it, so an operator can see it")
		assert.Equal(t, config.SourceEnv, byKey[key].From)
		assert.False(t, byKey[key].Overridable,
			"a variable pins the key, and the save handler says so")
	}
}

func TestLoad_SetupToken(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "test-secret-16+chars!")
	t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")

	t.Run("unset leaves it empty", func(t *testing.T) {
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Empty(t, cfg.SetupToken)
	})
	t.Run("long enough is kept", func(t *testing.T) {
		t.Setenv("LYEVE_SETUP_TOKEN", "operator-setup-token-0123")
		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, "operator-setup-token-0123", cfg.SetupToken)
	})
	t.Run("short is refused", func(t *testing.T) {
		t.Setenv("LYEVE_SETUP_TOKEN", "letmein")
		_, err := config.Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LYEVE_SETUP_TOKEN")
	})
}

func TestLoad_ConsoleKeyRotation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "a-strong-secret-for-the-config-tests")
	t.Setenv("ENCRYPTION_KEY", "another-strong-key-for-the-config-tests")
	current, previous := strings.Repeat("n", 32), strings.Repeat("o", 32)

	t.Setenv("ADMIN_CONSOLE_KEY", "")
	t.Setenv("ADMIN_CONSOLE_KEY_PREVIOUS", previous)
	_, err := config.Load()
	require.Error(t, err, "a previous key with no current one is a half-finished rotation")

	t.Setenv("ADMIN_CONSOLE_KEY", current)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte(current), []byte(previous)}, cfg.ConsoleKeys())

	t.Setenv("ADMIN_CONSOLE_KEY_PREVIOUS", "short")
	_, err = config.Load()
	require.Error(t, err)
}

func TestLoad_TLSFilesMustBeSetTogether(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "a-strong-secret-for-the-config-tests")
	t.Setenv("ENCRYPTION_KEY", "another-strong-key-for-the-config-tests")
	t.Setenv("TLS_CERT_FILE", "/etc/lyeve/tls.crt")
	t.Setenv("TLS_KEY_FILE", "")

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS_CERT_FILE and TLS_KEY_FILE")

	t.Setenv("TLS_KEY_FILE", "/etc/lyeve/tls.key")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "/etc/lyeve/tls.crt", cfg.TLSCertFile)
	assert.Equal(t, "/etc/lyeve/tls.key", cfg.TLSKeyFile)
}

func TestParseIssuerPolicies(t *testing.T) {
	issuers := []string{"https://idp.example.com"}
	for name, tc := range map[string]struct {
		raw     string
		wantErr string
	}{
		"empty":                   {"", ""},
		"valid":                   {`[{"issuer":"https://idp.example.com","tenant":"agency","roles_claim":"groups","role_map":{"cms-editors":"editor"},"default_roles":["viewer"]}]`, ""},
		"not json":                {`{issuer}`, "JSON array"},
		"unknown field":           {`[{"issuer":"https://idp.example.com","tenant":"agency","max_role":"admin"}]`, "JSON array"},
		"unlisted issuer":         {`[{"issuer":"https://other.example.com","tenant":"agency"}]`, "not in TRUSTED_ISSUERS"},
		"bad tenant":              {`[{"issuer":"https://idp.example.com","tenant":"Agency-1"}]`, "not a tenant slug"},
		"maps super_admin":        {`[{"issuer":"https://idp.example.com","tenant":"agency","role_map":{"root":"super_admin"}}]`, "cannot grant super_admin"},
		"defaults to super_admin": {`[{"issuer":"https://idp.example.com","tenant":"agency","default_roles":["super_admin"]}]`, "cannot grant super_admin"},
		"twice":                   {`[{"issuer":"https://idp.example.com","tenant":"agency"},{"issuer":"https://idp.example.com","tenant":"other"}]`, "two policies"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/cms")
			t.Setenv("JWT_SECRET", "a-strong-secret-for-the-config-tests")
			t.Setenv("ENCRYPTION_KEY", "another-strong-key-for-the-config-tests")
			t.Setenv("TRUSTED_ISSUERS", issuers[0])
			t.Setenv("TRUSTED_ISSUER_POLICIES", tc.raw)
			cfg, err := config.Load()
			if tc.wantErr == "" {
				require.NoError(t, err)
				if tc.raw != "" {
					require.Len(t, cfg.TrustedIssuerPolicies, 1)
					assert.Equal(t, "agency", cfg.TrustedIssuerPolicies[0].Tenant)
				}
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
