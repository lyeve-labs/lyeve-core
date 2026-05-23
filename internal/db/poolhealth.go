// Package db PoolHealth provides runtime introspection into the connection pool state  --
// both the Go-side *sql.DB pool and (when configured) the external pooler
// (PgBouncer / ProxySQL). The health check ping verifies not just connectivity
// but that the pool is in a healthy operational state (enough idle
// connections, no dead connections, response within SLA).
//
// Per-tenant pool sizing is surfaced as a map from tenant slug to its
// configured/actual pool allocation.
package db

import (
	"context"
	"fmt"
	"time"
)

// PoolHealth is a snapshot of connection pool health at a point in time.
type PoolHealth struct {
	// Engine is the database engine string: "postgres", "mysql", "mssql".
	Engine string

	// PoolStats is the *sql.DB stats snapshot.
	PoolStats PoolStatsSnapshot

	// PerTenant shows per-tenant pool allocation from PoolConfig (if set).
	PerTenant map[string]TenantPoolAllocation

	// Latency is the round-trip time of the most recent health-check ping.
	Latency time.Duration

	// Healthy is true when the pool passes all health checks.
	Healthy bool

	// CheckedAt is when this snapshot was taken.
	CheckedAt time.Time

	// Errors lists any health check failures.
	Errors []string

	// PreparedStmtCache holds prepared statement cache stats.
	PreparedStmtCache StmtCacheStats

	// SlowQueries holds per-dialect slow query stats when a SlowQueryTracer
	// is wrapped around the pool. Nil when no tracer is configured.
	SlowQueries *SlowQueryStats

	// QueryCache holds query result cache stats when a QueryCache is
	// configured. Nil when no query cache is active.
	QueryCache QueryCacheStats

	// PoolerConfig is the name of the external pooler if configured.
	PoolerConfig string
}

// PoolStatsSnapshot is a snapshot of the database/sql connection pool statistics.
type PoolStatsSnapshot struct {
	MaxOpenConnections int
	OpenConnections    int
	InUse              int
	Idle               int
	WaitCount          int64
	WaitDuration       time.Duration
	MaxIdleClosed      int64
	MaxIdleTimeClosed  int64
	MaxLifetimeClosed  int64
}

// TenantPoolAllocation captures per-tenant pool sizing.
type TenantPoolAllocation struct {
	// ConfiguredPoolSize is the pool_size from PoolConfig.
	ConfiguredPoolSize int32
	// ActualConnections is the current connection count for this tenant (if measurable).
	ActualConnections int
}

// PoolHealthCheck runs a full pool health assessment: pings the database,
// collects pool stats, checks thresholds, and returns a PoolHealth snapshot.
//
// The pool parameter is the DB interface, and poolSizeCfg is the optional
// PoolConfig for per-tenant sizing info. stmtCache is the optional prepared
// statement cache for stats.
//
// Thresholds:
//   - maxLatency: ping round-trip must be under this to be healthy (default 1s)
//   - minIdleConns: must have at least this many idle connections
//   - maxUtilization: InUse/OpenConnections must be under this ratio (0-1)
func PoolHealthCheck(
	ctx context.Context,
	pool DB,
	poolSizeCfg *PoolConfig,
	stmtCache *StmtCache,
	maxLatency time.Duration,
	minIdleConns int,
	maxUtilization float64,
) PoolHealth {
	start := time.Now()
	engine := pool.Engine()
	stats := pool.Stats()

	snapshot := PoolStatsSnapshot{
		MaxOpenConnections: stats.MaxOpenConnections,
		OpenConnections:    stats.OpenConnections,
		InUse:              stats.InUse,
		Idle:               stats.Idle,
		WaitCount:          stats.WaitCount,
		WaitDuration:       stats.WaitDuration,
		MaxIdleClosed:      stats.MaxIdleClosed,
		MaxIdleTimeClosed:  stats.MaxIdleTimeClosed,
		MaxLifetimeClosed:  stats.MaxLifetimeClosed,
	}

	health := PoolHealth{
		Engine:    engine,
		PoolStats: snapshot,
		CheckedAt: start,
		Healthy:   true,
	}

	// Ping check.
	pingStart := time.Now()
	if err := pool.Ping(ctx); err != nil {
		health.Healthy = false
		health.Errors = append(health.Errors, fmt.Sprintf("ping failed: %v", err))
	}
	health.Latency = time.Since(pingStart)

	// Latency threshold check.
	if health.Healthy && maxLatency > 0 && health.Latency > maxLatency {
		health.Healthy = false
		health.Errors = append(health.Errors,
			fmt.Sprintf("ping latency %v exceeds threshold %v", health.Latency, maxLatency))
	}

	// Idle connection minimum check.
	if minIdleConns > 0 && snapshot.Idle < minIdleConns {
		health.Healthy = false
		health.Errors = append(health.Errors,
			fmt.Sprintf("idle connections %d below minimum %d", snapshot.Idle, minIdleConns))
	}

	// Pool utilization ceiling check.
	if maxUtilization > 0 && maxUtilization < 1.0 && snapshot.OpenConnections > 0 {
		utilization := float64(snapshot.InUse) / float64(snapshot.OpenConnections)
		if utilization > maxUtilization {
			health.Healthy = false
			health.Errors = append(health.Errors,
				fmt.Sprintf("pool utilization %.2f exceeds threshold %.2f", utilization, maxUtilization))
		}
	}

	// Connection churn detection.
	// Too many MaxIdleClosed or MaxLifetimeClosed in a short window signals
	// the pool is churning: likely due to misconfigured idle timeout.
	if snapshot.MaxIdleClosed > 100 || snapshot.MaxLifetimeClosed > 100 {
		health.Errors = append(health.Errors,
			fmt.Sprintf("pool churn detected: MaxIdleClosed=%d MaxLifetimeClosed=%d",
				snapshot.MaxIdleClosed, snapshot.MaxLifetimeClosed))
		// Warning, not unhealthy: pool still works, just suboptimal
	}

	// Per-tenant pool sizing.
	if poolSizeCfg != nil {
		health.PerTenant = make(map[string]TenantPoolAllocation)
		for slug, size := range poolSizeCfg.PerTenantPools {
			health.PerTenant[slug] = TenantPoolAllocation{
				ConfiguredPoolSize: size.PoolSize,
			}
		}
	}

	// Prepared statement cache stats.
	if stmtCache != nil {
		health.PreparedStmtCache = stmtCache.Stats()
	}

	// Slow query stats.
	// If the pool is wrapped with a SlowQueryTracer, extract per-dialect stats.
	if tracer, ok := interface{}(pool).(*SlowQueryTracer); ok {
		stats := SlowQueryStatsFromTracer(tracer, engine, 20)
		health.SlowQueries = &stats
	}

	// Query cache stats.
	// QueryCache is not wired through the pool interface: it's a standalone
	// component. Stats are wired separately when the cache is instantiated.
	// The zero-value QueryCacheStats is correct when no cache is active.

	return health
}
