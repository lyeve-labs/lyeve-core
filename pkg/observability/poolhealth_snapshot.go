// Pool health extension for the Host interface.
//
// PoolHealth() on the Host interface lets the admin router serve
// /api/admin/pool/health without depending on internal/db directly.
// The runtime wires the real implementation via engineHost.

package observability

import "context"

// PoolHealthSnapshot is a public view of pool health for the admin dashboard.
// It mirrors internal/db.PoolHealth without importing internal packages.
type PoolHealthSnapshot struct {
	Engine            string                `json:"engine"`
	Healthy           bool                  `json:"healthy"`
	LatencyMs         float64               `json:"latency_ms"`
	OpenConns         int                   `json:"open_connections"`
	InUse             int                   `json:"in_use"`
	Idle              int                   `json:"idle"`
	MaxOpenConns      int                   `json:"max_open_connections"`
	WaitCount         int64                 `json:"wait_count"`
	WaitDurationMs    float64               `json:"wait_duration_ms"`
	MaxIdleClosed     int64                 `json:"max_idle_closed"`
	MaxLifetimeClosed int64                 `json:"max_lifetime_closed"`
	Errors            []string              `json:"errors,omitempty"`
	PerTenant         map[string]TenantPool `json:"per_tenant,omitempty"`
	StmtCacheSize     int                   `json:"stmt_cache_size,omitempty"`
	StmtCacheMaxSize  int                   `json:"stmt_cache_max_size,omitempty"`
	// SlowQueries holds per-dialect slow query stats when available.
	SlowQueries *SlowQuerySnapshot `json:"slow_queries,omitempty"`
	// QueryCache holds query result cache stats when available.
	QueryCacheSize int64 `json:"query_cache_size,omitempty"`
	QueryCacheHits int64 `json:"query_cache_hits,omitempty"`
	// Utilization is the pool utilization ratio (InUse / MaxOpenConns), 0-1.
	// 0 when MaxOpenConns is 0.
	Utilization float64 `json:"utilization"`
	// UtilizationWarning is true when utilization exceeds the exhaustion
	// threshold (default 80%). Operators use this to trigger alerts.
	UtilizationWarning bool `json:"utilization_warning"`
}

// SlowQuerySnapshot holds the public view of slow query tracking.
type SlowQuerySnapshot struct {
	TopByMaxDuration []SlowQueryRecordSnapshot `json:"top_by_max_duration"`
	TopByAvgDuration []SlowQueryRecordSnapshot `json:"top_by_avg_duration"`
	TrackedShapes    int                       `json:"tracked_shapes"`
}

// SlowQueryRecordSnapshot is a public view of a single slow query record.
type SlowQueryRecordSnapshot struct {
	NormalizedSQL string  `json:"normalized_sql"`
	Dialect       string  `json:"dialect"`
	Count         int64   `json:"count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	MaxDurationMs float64 `json:"max_duration_ms"`
	LastSeen      string  `json:"last_seen"`
}

// TenantPool is public per-tenant pool allocation for the health response.
type TenantPool struct {
	ConfiguredPoolSize int32 `json:"configured_pool_size"`
	ActualConnections  int   `json:"actual_connections,omitempty"`
}

// PoolHealthProvider is the capability interface for hosts that can report
// pool health. The admin router checks if the host implements this and
// mounts /api/admin/pool/health when present.
type PoolHealthProvider interface {
	// PoolHealth returns a snapshot of the connection pool health.
	PoolHealth(ctx context.Context) PoolHealthSnapshot
}
