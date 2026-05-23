package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPoolConfig(t *testing.T) {
	cfg := DefaultPoolConfig()
	assert.Equal(t, int32(100), cfg.MaxClientConn)
	assert.Equal(t, int32(20), cfg.DefaultPoolSize)
	assert.Equal(t, int32(5), cfg.ReservePoolSize)
	assert.Equal(t, int32(5), cfg.ReservePoolTimeout)
	assert.Equal(t, int32(100), cfg.MaxDBConnections)
	assert.Equal(t, int32(50), cfg.MaxUserConnections)

	require.Contains(t, cfg.PerTenantPools, "default")
	def := cfg.PerTenantPools["default"]
	assert.Equal(t, int32(20), def.PoolSize)
	assert.Equal(t, int32(5), def.MinPoolSize)
	assert.Equal(t, int32(50), def.MaxConnections)
	assert.Equal(t, "cms", def.DBName)
}

func TestTenantSize_Fallback(t *testing.T) {
	cfg := PoolConfig{
		PerTenantPools: map[string]TenantPoolSize{
			"default": {PoolSize: 20, MinPoolSize: 5},
			"acme":    {PoolSize: 50, MinPoolSize: 10},
		},
	}

	// Explicit tenant
	assert.Equal(t, int32(50), cfg.TenantSize("acme").PoolSize)
	assert.Equal(t, int32(10), cfg.TenantSize("acme").MinPoolSize)

	// Unknown tenant -> default
	assert.Equal(t, int32(20), cfg.TenantSize("beta").PoolSize)
	assert.Equal(t, int32(5), cfg.TenantSize("beta").MinPoolSize)

	// No default set -> zero value
	empty := PoolConfig{PerTenantPools: map[string]TenantPoolSize{}}
	assert.Equal(t, int32(0), empty.TenantSize("anything").PoolSize)
}

func TestResolveDBName(t *testing.T) {
	cfg := PoolConfig{
		PerTenantPools: map[string]TenantPoolSize{
			"default": {DBName: ""},            // falls back
			"acme":    {DBName: "tenant_acme"}, // explicit
		},
	}
	assert.Equal(t, "cms", cfg.ResolveDBName("beta", "cms"))
	assert.Equal(t, "tenant_acme", cfg.ResolveDBName("acme", "cms"))
}

func TestValidate_Valid(t *testing.T) {
	cfg := PoolConfig{
		MaxClientConn:   100,
		DefaultPoolSize: 20,
		PerTenantPools: map[string]TenantPoolSize{
			"default": {PoolSize: 20},
			"acme":    {PoolSize: 30},
			"beta":    {PoolSize: 10},
		},
	}
	assert.NoError(t, cfg.Validate()) // 20+30+10 = 60 < 100
}

func TestValidate_Errors(t *testing.T) {
	t.Run("negative MaxClientConn", func(t *testing.T) {
		cfg := PoolConfig{MaxClientConn: -1}
		assert.ErrorContains(t, cfg.Validate(), "MaxClientConn must be positive")
	})

	t.Run("zero DefaultPoolSize", func(t *testing.T) {
		cfg := PoolConfig{MaxClientConn: 100}
		assert.ErrorContains(t, cfg.Validate(), "DefaultPoolSize must be positive")
	})

	t.Run("DefaultPoolSize exceeds MaxClientConn", func(t *testing.T) {
		cfg := PoolConfig{MaxClientConn: 10, DefaultPoolSize: 20}
		assert.ErrorContains(t, cfg.Validate(), "cannot exceed MaxClientConn")
	})

	t.Run("tenant PoolSize exceeds MaxConnections", func(t *testing.T) {
		cfg := PoolConfig{
			MaxClientConn:   100,
			DefaultPoolSize: 20,
			PerTenantPools: map[string]TenantPoolSize{
				"default": {PoolSize: 20},
				"acme":    {PoolSize: 30, MaxConnections: 10}, // PoolSize > MaxConnections
			},
		}
		assert.ErrorContains(t, cfg.Validate(), "cannot exceed MaxConnections")
	})

	t.Run("total exceeds MaxClientConn", func(t *testing.T) {
		cfg := PoolConfig{
			MaxClientConn:   50,
			DefaultPoolSize: 20,
			PerTenantPools: map[string]TenantPoolSize{
				"default": {PoolSize: 20},
				"acme":    {PoolSize: 30},
				"beta":    {PoolSize: 20}, // 20+30+20 = 70 > 50
			},
		}
		assert.ErrorContains(t, cfg.Validate(), "exceeds MaxClientConn")
	})
}
