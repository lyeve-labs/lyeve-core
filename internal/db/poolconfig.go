// Package db PoolConfig captures per-tenant pool sizing so operators can
// allocate dedicated connection resources per tenant. The engine reports the
// sizing on /api/admin/pool/health and uses the thresholds to decide when a
// pool is degraded. It does not configure the pooler itself.
package db

import "fmt"

// PoolConfig captures pool sizing parameters shared across PgBouncer, ProxySQL,
// and the built-in connection pool. PerTenantPools maps tenant slugs to their
// dedicated pool allocations. The "default" key provides the fallback for tenants
// without explicit sizing.
type PoolConfig struct {
	// PerTenantPools maps tenant slug -> pool sizing. Key "default" is special:
	// its values apply to any tenant not explicitly listed here. Typically
	// small tenants share the default pool. Large tenants get dedicated.
	PerTenantPools map[string]TenantPoolSize

	// MaxClientConn is the global cap on client-side connections accepted by the
	// pooler. Must be >= sum of all tenant pool sizes + overhead.
	MaxClientConn int32

	// DefaultPoolSize is the pooler-level default pool_size. Applied when
	// no per-database override is present (PgBouncer) or as hostgroup default
	// (ProxySQL).
	DefaultPoolSize int32

	// ReservePoolSize is the pooler-level reserve_pool_size: connections
	// kept in reserve when the main pool is exhausted (PgBouncer).
	ReservePoolSize int32

	// ReservePoolTimeout is how long (seconds) a client waits in the reserve
	// pool before the pooler returns an error.
	ReservePoolTimeout int32

	// MaxDBConnections is the maximum number of physical connections the pooler
	// opens to each database host. Only meaningful in transaction pooling mode.
	MaxDBConnections int32

	// MaxUserConnections caps per-user connections in the pooler.
	MaxUserConnections int32
}

// TenantPoolSize captures the pool allocation for a single tenant.
type TenantPoolSize struct {
	// PoolSize is max connections for this tenant (per-pool / per-hostgroup).
	PoolSize int32

	// MinPoolSize is the minimum number of connections kept warm in the pool.
	MinPoolSize int32

	// MaxConnections caps total connections for this tenant across all hosts.
	MaxConnections int32

	// DBName is the physical database/schema name for this tenant.
	// For Postgres schema-per-tenant this is the shared database. For
	// MySQL/MSSQL database-per-tenant this is "tenant_<slug>".
	DBName string
}

// DefaultPoolConfig returns sensible defaults for a typical deployment.
// Operators should tune PerTenantPools for their workload.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		PerTenantPools: map[string]TenantPoolSize{
			"default": {
				PoolSize:       20,
				MinPoolSize:    5,
				MaxConnections: 50,
				DBName:         "cms",
			},
		},
		MaxClientConn:      100,
		DefaultPoolSize:    20,
		ReservePoolSize:    5,
		ReservePoolTimeout: 5,
		MaxDBConnections:   100,
		MaxUserConnections: 50,
	}
}

// Per-tenant pool sizing helpers.

// TenantSize resolves the TenantPoolSize for the given slug, falling back to
// the "default" key when no explicit entry exists. Returns zero-value
// TenantPoolSize when no "default" key is set either: callers should treat
// zero values as "use engine defaults".
func (cfg PoolConfig) TenantSize(slug string) TenantPoolSize {
	if s, ok := cfg.PerTenantPools[slug]; ok {
		return s
	}
	return cfg.PerTenantPools["default"]
}

// ResolveDBName returns the tenant database/schema name for configuration
// generation purposes. Falls back to the provided defaultDBName.
func (cfg PoolConfig) ResolveDBName(slug, defaultDBName string) string {
	s := cfg.TenantSize(slug)
	if s.DBName != "" {
		return s.DBName
	}
	return defaultDBName
}

// Validate checks PoolConfig for obvious misconfigurations. Returns nil when
// the config is likely correct, or an error describing the first issue found.
func (cfg PoolConfig) Validate() error {
	if cfg.MaxClientConn <= 0 {
		return fmt.Errorf("MaxClientConn must be positive, got %d", cfg.MaxClientConn)
	}
	if cfg.DefaultPoolSize <= 0 {
		return fmt.Errorf("DefaultPoolSize must be positive, got %d", cfg.DefaultPoolSize)
	}
	if cfg.DefaultPoolSize > cfg.MaxClientConn {
		return fmt.Errorf("DefaultPoolSize (%d) cannot exceed MaxClientConn (%d)", cfg.DefaultPoolSize, cfg.MaxClientConn)
	}
	var totalPerTenant int32
	for slug, s := range cfg.PerTenantPools {
		if s.PoolSize <= 0 {
			return fmt.Errorf("tenant %q: PoolSize must be positive, got %d", slug, s.PoolSize)
		}
		if s.MaxConnections > 0 && s.PoolSize > s.MaxConnections {
			return fmt.Errorf("tenant %q: PoolSize (%d) cannot exceed MaxConnections (%d)", slug, s.PoolSize, s.MaxConnections)
		}
		totalPerTenant += s.PoolSize
	}
	if totalPerTenant > cfg.MaxClientConn {
		return fmt.Errorf("sum of per-tenant PoolSize (%d) exceeds MaxClientConn (%d)", totalPerTenant, cfg.MaxClientConn)
	}
	return nil
}
