// engineHost PoolHealth integration.
//
// engineHost implements observability.PoolHealthProvider by delegating to internal/db pool
// health checks. Exposes PoolHealth() on the host for the admin router's
// /api/admin/pool/health endpoint.

package enginehost

import (
	"context"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/observability"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// observability.PoolHealthProvider implementation

// Ensure engineHost satisfies observability.PoolHealthProvider when poolCfg is wired.
var _ observability.PoolHealthProvider = (*engineHost)(nil)

// poolHealthCfg caches the pool health configuration wired at startup.
// Set via WithPoolHealth method.
type poolHealthCfg struct {
	poolCfg        *db.PoolConfig
	stmtCache      *db.StmtCache
	poolMonitor    *db.PoolHealthMonitor // nil = check on every call
	maxLatency     time.Duration
	minIdle        int
	maxUtilization float64
}

// WithPoolHealth wires pool health configuration into the host.
// Called by the runtime after the pool config and statement cache are created.
func (h *engineHost) WithPoolHealth(
	poolCfg *db.PoolConfig,
	stmtCache *db.StmtCache,
	maxLatency time.Duration,
	minIdle int,
	maxUtilization float64,
) {
	h.poolHealth = &poolHealthCfg{
		poolCfg:        poolCfg,
		stmtCache:      stmtCache,
		maxLatency:     maxLatency,
		minIdle:        minIdle,
		maxUtilization: maxUtilization,
	}
}

// WithPoolHealthMonitor wires a background pool health monitor so
// PoolHealth() returns the latest background snapshot instead of doing
// a synchronous ping on every call. Prefer this for production: it keeps
// health checks from adding latency to admin requests.
func (h *engineHost) WithPoolHealthMonitor(monitor *db.PoolHealthMonitor) {
	if h.poolHealth == nil {
		h.poolHealth = &poolHealthCfg{}
	}
	h.poolHealth.poolMonitor = monitor
}

// PoolHealth returns a snapshot of the connection pool health.
// Satisfies the observability.PoolHealthProvider interface.
//
// When a background PoolHealthMonitor is wired, this returns the latest
// cached snapshot without blocking. Otherwise it runs a synchronous
// health check (which includes a ping: use the monitor for production).
func (h *engineHost) PoolHealth(ctx context.Context) observability.PoolHealthSnapshot {
	if h.poolHealth == nil {
		return observability.PoolHealthSnapshot{
			Healthy: true,
			Errors:  []string{"pool health not configured"},
		}
	}

	// Fast path: return latest background snapshot.
	if h.poolHealth.poolMonitor != nil {
		return snapshotFromRaw(h.poolHealth.poolMonitor.Latest())
	}

	raw := db.PoolHealthCheck(
		ctx,
		h.pool,
		h.poolHealth.poolCfg,
		h.poolHealth.stmtCache,
		h.poolHealth.maxLatency,
		h.poolHealth.minIdle,
		h.poolHealth.maxUtilization,
	)

	return snapshotFromRaw(raw)
}

// snapshotFromRaw converts a db.PoolHealth into the public observability.PoolHealthSnapshot.
func snapshotFromRaw(raw db.PoolHealth) observability.PoolHealthSnapshot {
	snapshot := observability.PoolHealthSnapshot{
		Engine:            raw.Engine,
		Healthy:           raw.Healthy,
		LatencyMs:         float64(raw.Latency.Microseconds()) / 1000.0,
		OpenConns:         raw.PoolStats.OpenConnections,
		InUse:             raw.PoolStats.InUse,
		Idle:              raw.PoolStats.Idle,
		MaxOpenConns:      raw.PoolStats.MaxOpenConnections,
		WaitCount:         raw.PoolStats.WaitCount,
		WaitDurationMs:    float64(raw.PoolStats.WaitDuration.Microseconds()) / 1000.0,
		MaxIdleClosed:     raw.PoolStats.MaxIdleClosed,
		MaxLifetimeClosed: raw.PoolStats.MaxLifetimeClosed,
		Errors:            raw.Errors,
		StmtCacheSize:     raw.PreparedStmtCache.Size,
		StmtCacheMaxSize:  raw.PreparedStmtCache.MaxSize,
	}

	// Compute utilization ratio + exhaustion warning.
	if raw.PoolStats.MaxOpenConnections > 0 {
		snapshot.Utilization = float64(raw.PoolStats.InUse) / float64(raw.PoolStats.MaxOpenConnections)
		// Use the same threshold as the Prometheus gauge (metrics.ExhaustionThreshold).
		// Import cycle prevents direct reference: use the db-level maxUtil
		// if available, otherwise default to 0.8.
		const defaultExhaustionThreshold = 0.8
		if snapshot.Utilization > defaultExhaustionThreshold {
			snapshot.UtilizationWarning = true
		}
	}

	if raw.SlowQueries != nil {
		sq := raw.SlowQueries
		snapshot.SlowQueries = &observability.SlowQuerySnapshot{
			TrackedShapes: sq.TrackedShapes,
		}
		for _, r := range sq.TopByMaxDuration {
			snapshot.SlowQueries.TopByMaxDuration = append(snapshot.SlowQueries.TopByMaxDuration, slowRecordFromRaw(r))
		}
		for _, r := range sq.TopByAvgDuration {
			snapshot.SlowQueries.TopByAvgDuration = append(snapshot.SlowQueries.TopByAvgDuration, slowRecordFromRaw(r))
		}
	}

	snapshot.QueryCacheSize = int64(raw.QueryCache.Size)
	snapshot.QueryCacheHits = raw.QueryCache.TotalHits

	if raw.PerTenant != nil {
		snapshot.PerTenant = make(map[string]observability.TenantPool, len(raw.PerTenant))
		for slug, alloc := range raw.PerTenant {
			snapshot.PerTenant[slug] = observability.TenantPool{
				ConfiguredPoolSize: alloc.ConfiguredPoolSize,
				ActualConnections:  alloc.ActualConnections,
			}
		}
	}

	return snapshot
}

func slowRecordFromRaw(r db.SlowQueryRecord) observability.SlowQueryRecordSnapshot {
	return observability.SlowQueryRecordSnapshot{
		NormalizedSQL: r.NormalizedSQL,
		Dialect:       r.Dialect,
		Count:         r.Count,
		AvgDurationMs: float64(r.AvgDuration.Microseconds()) / 1000.0,
		MaxDurationMs: float64(r.MaxDuration.Microseconds()) / 1000.0,
		LastSeen:      r.LastSeen.Format(time.RFC3339),
	}
}
