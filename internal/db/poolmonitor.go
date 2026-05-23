// Package db PoolHealthMonitor runs a background goroutine that periodically calls
// PoolHealthCheck and stores the latest snapshot. The engineHost's
// PoolHealth() method reads this snapshot so /api/admin/pool/health always
// returns the most recent check without blocking on a ping.
//
// The monitor also surfaces per-tenant pool allocation from PoolConfig,
// enabling observability into per-tenant connection budgets at runtime.
package db

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// PoolHealthMonitor runs periodic pool health checks and caches the latest
// snapshot. It implements a simple store/latest pattern: the background
// goroutine updates a mutex-protected snapshot and the Latest() method
// returns it without blocking.
type PoolHealthMonitor struct {
	pool        DB
	poolSizeCfg *PoolConfig
	stmtCache   *StmtCache
	maxLatency  time.Duration
	minIdle     int
	maxUtil     float64
	interval    time.Duration

	mu       sync.RWMutex
	snapshot PoolHealth
	done     chan struct{}
}

// PoolMonitorConfig carries all parameters needed to start monitoring.
type PoolMonitorConfig struct {
	Pool          DB
	PoolSizeCfg   *PoolConfig
	StmtCache     *StmtCache
	MaxLatency    time.Duration
	MinIdle       int
	MaxUtil       float64
	CheckInterval time.Duration
}

// NewPoolHealthMonitor creates a background pool health monitor. The monitor
// does not start automatically: call Start(ctx) to begin periodic checks.
//
// When poolSizeCfg is nil, per-tenant sizing is omitted from snapshots.
// When stmtCache is nil, prepared statement cache stats are omitted.
func NewPoolHealthMonitor(cfg PoolMonitorConfig) *PoolHealthMonitor {
	return &PoolHealthMonitor{
		pool:        cfg.Pool,
		poolSizeCfg: cfg.PoolSizeCfg,
		stmtCache:   cfg.StmtCache,
		maxLatency:  cfg.MaxLatency,
		minIdle:     cfg.MinIdle,
		maxUtil:     cfg.MaxUtil,
		interval:    cfg.CheckInterval,
		done:        make(chan struct{}),
	}
}

// Start begins periodic pool health checks. The first check runs immediately
// (before Start returns) so callers can read a valid snapshot right after.
// Run checks happen every interval. Stops on ctx.Done() or Stop().
func (m *PoolHealthMonitor) Start(ctx context.Context) {
	// First check is synchronous: ensures a valid snapshot exists before
	// returning so the /health endpoint never returns empty data.
	m.runCheck(ctx)
	go m.loop(ctx)
}

// Stop signals the background goroutine to exit. Does not wait: the
// goroutine exits on its own once it notices the done channel or ctx.
func (m *PoolHealthMonitor) Stop() {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
}

// Latest returns the most recent health check snapshot. Always returns a
// valid snapshot after Start() has been called.
func (m *PoolHealthMonitor) Latest() PoolHealth {
	m.mu.RLock()
	s := m.snapshot
	m.mu.RUnlock()
	return s
}

// loop is the background ticker-based check loop.
func (m *PoolHealthMonitor) loop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case <-ticker.C:
			m.runCheck(ctx)
		}
	}
}

// defaultCheckLatency is used to size the per-check timeout when maxLatency
// is unset or invalid, so the timeout never collapses to an already-expired
// context.
const defaultCheckLatency = 1 * time.Second

// runCheck executes a single health check and stores the result.
func (m *PoolHealthMonitor) runCheck(ctx context.Context) {
	// Use a short timeout per check so a slow database doesn't
	// cause the monitor goroutine to block indefinitely.
	latency := m.maxLatency
	if latency <= 0 {
		latency = defaultCheckLatency
	}
	checkCtx, cancel := context.WithTimeout(ctx, latency*5)
	defer cancel()

	health := PoolHealthCheck(
		checkCtx,
		m.pool,
		m.poolSizeCfg,
		m.stmtCache,
		m.maxLatency,
		m.minIdle,
		m.maxUtil,
	)

	if !health.Healthy {
		slog.Warn("pool health check failed",
			"engine", health.Engine,
			"errors", health.Errors,
			"latency_ms", float64(health.Latency.Microseconds())/1000.0,
			"open_conns", health.PoolStats.OpenConnections,
			"in_use", health.PoolStats.InUse,
		)
	}

	m.mu.Lock()
	m.snapshot = health
	m.mu.Unlock()
}

// Ensure PoolHealthMonitor satisfies the interface expected by engineHost
// when PoolHealth delegates to it. The host reads Latest() directly.
var _ interface{ Latest() PoolHealth } = (*PoolHealthMonitor)(nil)
