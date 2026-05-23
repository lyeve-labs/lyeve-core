// Package middleware three layers of overload protection:
//  1. Global concurrency cap: weighted semaphore limits in-flight requests
//     (503 + Retry-After when saturated).
//  2. Per-tenant fair share: proportional allocation prevents starvation.
//  3. DB-pool-pressure-aware shedding: polls *sql.DB stats, sheds before
//     pool exhaustion.
//
// Health/ready/metrics endpoints bypass all shedding.
package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// PoolStatsFunc returns current database pool statistics. Typically (*sql.DB).Stats.
type PoolStatsFunc func() PoolStats

// PoolStats mirrors the subset of sql.DBStats needed for backpressure decisions.
type PoolStats struct {
	MaxOpenConnections int
	OpenConnections    int
	InUse              int
	WaitCount          int64
	WaitDuration       time.Duration
}

// BackpressureConfig controls the backpressure middleware behavior.
type BackpressureConfig struct {
	// MaxInflight is the global cap on concurrent in-flight requests.
	// When this many requests are being served, new ones get 503.
	// Default: 200.
	MaxInflight int64

	// TenantQuotaPct is the maximum fraction of MaxInflight that a
	// single tenant may consume. Range (0, 1]. Set to 1.0 to disable
	// per-tenant fair share. Default: 0.4 (40%).
	TenantQuotaPct float64

	// PoolStats, when non-nil, enables DB-pool-pressure-aware shedding.
	// The middleware polls this function at PoolPollInterval.
	PoolStats PoolStatsFunc

	// PoolPressureThreshold is the fraction of MaxOpenConnections at
	// which the middleware starts shedding. Range (0, 1].
	// Default: 0.85 (85%).
	PoolPressureThreshold float64

	// PoolPollInterval controls how often PoolStats is sampled.
	// Default: 500ms. Lower values react faster but cost more CPU.
	PoolPollInterval time.Duration

	// RetryAfterSeconds is the Retry-After header value sent on 503.
	// Default: 1.
	RetryAfterSeconds int

	// BypassPaths are URL path prefixes that skip backpressure.
	// Default: health, ready, startup, metrics.
	BypassPaths []string
}

// BackpressureResult bundles the middleware with lifecycle controls.
type BackpressureResult struct {
	// Middleware is the chi-compatible middleware to apply.
	Middleware func(http.Handler) http.Handler

	// Stop terminates the background pool-stats poller. Safe to call
	// multiple times. Call on server shutdown.
	Stop func()

	// Stats returns a snapshot of the backpressure state for
	// observability endpoints.
	Stats func() BackpressureStats
}

// BackpressureStats is a point-in-time snapshot of backpressure state.
type BackpressureStats struct {
	// GlobalInflight is the current number of in-flight requests.
	GlobalInflight int64 `json:"global_inflight"`

	// GlobalMax is the configured MaxInflight.
	GlobalMax int64 `json:"global_max"`

	// PoolPressure is the current pool utilization ratio (0..1+).
	// 0 when no PoolStats func is configured.
	PoolPressure float64 `json:"pool_pressure"`

	// Shedding is true if pool-pressure shedding is currently active.
	Shedding bool `json:"shedding"`

	// PerTenantInflight shows in-flight counts for active tenants.
	PerTenantInflight map[string]int64 `json:"per_tenant_inflight,omitempty"`
}

// tenantSlot tracks per-tenant in-flight requests.
type tenantSlot struct {
	count atomic.Int64
}

// Backpressure creates a backpressure/load-shedding middleware.
//
// Three independent protection layers:
//
//  1. Global concurrency: semaphore(maxInflight) -> 503 on saturation.
//  2. Per-tenant fair share: per-tenant atomic counter, cap = maxInflight * quotaPct.
//  3. DB pool pressure: when InUse/MaxOpen > threshold, shed with 503.
//
// Returns an error if config validation fails.
func Backpressure(cfg BackpressureConfig) (BackpressureResult, error) {
	// Defaults
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 200
	}
	if cfg.TenantQuotaPct <= 0 || cfg.TenantQuotaPct > 1 {
		cfg.TenantQuotaPct = 0.4
	}
	if cfg.PoolPressureThreshold <= 0 || cfg.PoolPressureThreshold > 1 {
		cfg.PoolPressureThreshold = 0.85
	}
	if cfg.PoolPollInterval <= 0 {
		cfg.PoolPollInterval = 500 * time.Millisecond
	}
	if cfg.RetryAfterSeconds <= 0 {
		cfg.RetryAfterSeconds = 1
	}
	if len(cfg.BypassPaths) == 0 {
		cfg.BypassPaths = defaultBackpressureBypass
	}

	tenantCap := int64(math.Ceil(float64(cfg.MaxInflight) * cfg.TenantQuotaPct))
	if tenantCap < 1 {
		tenantCap = 1
	}

	var (
		globalSem      = semaphore.NewWeighted(cfg.MaxInflight)
		tenants        sync.Map // map[string]*tenantSlot
		globalInflight atomic.Int64

		// Pool pressure state, updated by the background poller.
		poolPressure atomic.Int64 // pressure * 1000 (fixed-point, 3 decimals)
		poolShedding atomic.Bool

		stopCh  = make(chan struct{})
		stopped atomic.Bool
	)

	// Background pool-stats poller
	if cfg.PoolStats != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("backpressure pool-stats poller panicked", "panic", r)
				}
			}()
			ticker := time.NewTicker(cfg.PoolPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
				}
				stats := cfg.PoolStats()
				if stats.MaxOpenConnections <= 0 {
					poolPressure.Store(0)
					poolShedding.Store(false)
					continue
				}
				pressure := float64(stats.InUse) / float64(stats.MaxOpenConnections)
				poolPressure.Store(int64(pressure * 1000))
				poolShedding.Store(pressure >= cfg.PoolPressureThreshold)
			}
		}()
	}

	stopFn := func() {
		if stopped.CompareAndSwap(false, true) {
			close(stopCh)
		}
	}

	bypassSet := make(map[string]struct{}, len(cfg.BypassPaths))
	for _, p := range cfg.BypassPaths {
		bypassSet[p] = struct{}{}
	}

	retryAfterStr := strconv.Itoa(cfg.RetryAfterSeconds)

	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Bypass health/ready/metrics
			if isBackpressureBypass(r.URL.Path, bypassSet) {
				next.ServeHTTP(w, r)
				return
			}

			// Layer 3: DB pool pressure (cheapest check first)
			if poolShedding.Load() {
				w.Header().Set("Retry-After", retryAfterStr)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Shedding-Reason", "pool_pressure")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w,
					`{"error":"service overloaded","reason":"database pool pressure","retry_after":%s}`,
					retryAfterStr)
				return
			}

			// Layer 2: Per-tenant fair share
			tenantID := TenantIDFromCtx(r)
			if tenantID == "" {
				tenantID = "_anonymous"
			}

			var ts *tenantSlot
			val, ok := tenants.Load(tenantID)
			if !ok {
				newSlot := &tenantSlot{}
				actual, _ := tenants.LoadOrStore(tenantID, newSlot)
				ts = actual.(*tenantSlot)
			} else {
				ts = val.(*tenantSlot)
			}

			current := ts.count.Load()
			if current >= tenantCap {
				w.Header().Set("Retry-After", retryAfterStr)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Shedding-Reason", "tenant_quota")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w,
					`{"error":"service overloaded","reason":"tenant concurrency quota exceeded","retry_after":%s}`,
					retryAfterStr)
				return
			}

			// Layer 1: Global concurrency cap
			ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
			defer cancel()
			if err := globalSem.Acquire(ctx, 1); err != nil {
				// Acquire failed (timeout or cancellation): shed.
				w.Header().Set("Retry-After", retryAfterStr)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Shedding-Reason", "global_concurrency")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w,
					`{"error":"service overloaded","reason":"server at capacity","retry_after":%s}`,
					retryAfterStr)
				return
			}

			// Acquired: track in-flight
			ts.count.Add(1)
			globalInflight.Add(1)

			defer func() {
				ts.count.Add(-1)
				globalInflight.Add(-1)
				globalSem.Release(1)
			}()

			next.ServeHTTP(w, r)
		})
	}

	statsFn := func() BackpressureStats {
		s := BackpressureStats{
			GlobalInflight: globalInflight.Load(),
			GlobalMax:      cfg.MaxInflight,
			PoolPressure:   float64(poolPressure.Load()) / 1000.0,
			Shedding:       poolShedding.Load(),
		}
		// Collect per-tenant inflight (best-effort).
		pt := make(map[string]int64)
		tenants.Range(func(key, value any) bool {
			k := key.(string)
			v := value.(*tenantSlot)
			c := v.count.Load()
			if c > 0 {
				pt[k] = c
			}
			return true
		})
		if len(pt) > 0 {
			s.PerTenantInflight = pt
		}
		return s
	}

	return BackpressureResult{
		Middleware: mw,
		Stop:       stopFn,
		Stats:      statsFn,
	}, nil
}

// defaultBackpressureBypass lists path prefixes that skip backpressure.
//
// The probes are here because shedding them makes an orchestrator kill a node
// that is merely busy. `/.well-known/` is here for the same kind of reason:
// JWKS is how an external service verifies a token this engine issued, it
// serves a cached public key and touches no database, and shedding it turns a
// load spike into an authentication outage across every service that depends
// on this one. Unauthenticated callers also share a single `_anonymous` tenant
// slot, so without the bypass JWKS would compete for that quota with every
// other anonymous request and be among the first things dropped.
var defaultBackpressureBypass = []string{
	"/healthz", "/readyz", "/startup", "/health", "/ready", "/metrics",
	"/.well-known/",
}

// isBackpressureBypass returns true if the path matches any bypass prefix.
func isBackpressureBypass(path string, bypassSet map[string]struct{}) bool {
	for prefix := range bypassSet {
		if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
