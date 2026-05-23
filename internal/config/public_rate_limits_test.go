package config_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadWithLimits loads a minimal config with the two public rate limit settings
// applied, so the assertions below are about those settings alone.
func loadWithLimits(t *testing.T, perRoute, global string) *config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "test-secret-16+chars")
	t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
	if perRoute != "" {
		t.Setenv("PUBLIC_RATE_LIMITS", perRoute)
	}
	if global != "" {
		t.Setenv("PUBLIC_RATE_LIMIT_GLOBAL", global)
	}

	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg
}

func TestLoad_PublicRateLimitsAbsentLeavesTheEngineDefaults(t *testing.T) {
	t.Setenv("PUBLIC_RATE_LIMITS", "")
	t.Setenv("PUBLIC_RATE_LIMIT_GLOBAL", "")

	cfg := loadWithLimits(t, "", "")

	assert.Empty(t, cfg.PublicRateLimits, "an operator who sets nothing overrides nothing")
	assert.Empty(t, cfg.PublicRateLimitGlobal)
}

func TestLoad_PublicRateLimitsAreRead(t *testing.T) {
	cfg := loadWithLimits(t,
		"POST:/api/v1/flows/hooks/{flow_id}=20:100, GET:/api/v1/flows/p/{flow_id}=10:50",
		" 200:400 ")

	assert.Equal(t, []string{
		"POST:/api/v1/flows/hooks/{flow_id}=20:100",
		"GET:/api/v1/flows/p/{flow_id}=10:50",
	}, cfg.PublicRateLimits, "entries are split on commas and trimmed, braces intact")
	assert.Equal(t, "200:400", cfg.PublicRateLimitGlobal)
}
