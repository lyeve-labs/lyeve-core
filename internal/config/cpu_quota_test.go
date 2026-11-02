package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// CPU_QUOTA is read from the environment only. The engine no longer derives
// it from the cgroup, because Go's default already follows the CPU limit.
func TestLoad_CPUQuota(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/cms")
	t.Setenv("JWT_SECRET", "test-secret-16+chars!")
	t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")

	t.Setenv("CPU_QUOTA", "")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.CPUQuota)

	t.Setenv("CPU_QUOTA", "3")
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.CPUQuota)

	for _, bad := range []string{"two", "-1"} {
		t.Setenv("CPU_QUOTA", bad)
		_, err = config.Load()
		assert.Error(t, err, bad)
	}
}
