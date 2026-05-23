// Package middleware LatencyTracker records request durations per endpoint (method+path) using
// a bounded in-memory reservoir for p50/p95/p99 percentile calculation.
// Cheap: single sync.Map lookup and append on the hot path, no extra
// allocations beyond the duration value.
package middleware

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// EndpointLatency holds per-endpoint latency statistics with a ring-buffer
// of recent samples for accurate percentile calculation.
type EndpointLatency struct {
	mu sync.Mutex

	Method string // HTTP method (GET, POST, etc.)
	Path   string // Route pattern (e.g. /api/v1/content/{schema})

	Count   uint64 // total requests
	MinUS   uint64 // minimum observed latency in microseconds
	MaxUS   uint64 // maximum observed latency in microseconds
	TotalUS uint64 // sum of all latencies (for average)

	// Ring buffer of recent samples for percentile calculation.
	buffer []uint64 // microseconds
	head   int      // next write position
}

// record adds a latency sample to the ring buffer and updates aggregates.
func (e *EndpointLatency) record(durUs uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.Count++
	e.TotalUS += durUs
	if e.MinUS == 0 || durUs < e.MinUS {
		e.MinUS = durUs
	}
	if durUs > e.MaxUS {
		e.MaxUS = durUs
	}

	if len(e.buffer) > 0 {
		e.buffer[e.head] = durUs
		e.head = (e.head + 1) % len(e.buffer)
	}
}

// Stats returns computed statistics from the ring buffer.
func (e *EndpointLatency) Stats() LatencyStats {
	e.mu.Lock()
	defer e.mu.Unlock()

	stats := LatencyStats{
		Method:  e.Method,
		Path:    e.Path,
		Count:   e.Count,
		MinUS:   e.MinUS,
		MaxUS:   e.MaxUS,
		AvgUS:   0,
		P50US:   0,
		P95US:   0,
		P99US:   0,
		Samples: len(e.buffer),
	}

	if e.Count > 0 {
		stats.AvgUS = e.TotalUS / e.Count
	}

	if len(e.buffer) == 0 {
		return stats
	}

	// Copy and sort the ring buffer for percentile calculation.
	live := e.liveSamplesLocked()
	if len(live) == 0 {
		return stats
	}

	sort.Slice(live, func(i, j int) bool { return live[i] < live[j] })
	stats.P50US = live[percentileIdx(len(live), 0.50)]
	stats.P95US = live[percentileIdx(len(live), 0.95)]
	stats.P99US = live[percentileIdx(len(live), 0.99)]
	return stats
}

// liveSamplesLocked returns a copy of non-zero samples from the ring buffer.
// Must be called with e.mu held.
func (e *EndpointLatency) liveSamplesLocked() []uint64 {
	filled := len(e.buffer)
	if e.Count < uint64(filled) {
		filled = int(e.Count)
	}
	if filled == 0 {
		return nil
	}
	out := make([]uint64, filled)
	// Walk backwards from (head-1): head points to next write slot.
	for i := 0; i < filled; i++ {
		idx := (e.head - 1 - i + len(e.buffer)) % len(e.buffer)
		out[i] = e.buffer[idx]
	}
	return out
}

// LatencyStats is the computed statistics for an endpoint.
type LatencyStats struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Count   uint64 `json:"count"`
	MinUS   uint64 `json:"min_us"`
	MaxUS   uint64 `json:"max_us"`
	AvgUS   uint64 `json:"avg_us"`
	P50US   uint64 `json:"p50_us"`
	P95US   uint64 `json:"p95_us"`
	P99US   uint64 `json:"p99_us"`
	Samples int    `json:"samples"`
}

// LatencyTracker records per-endpoint request latencies.
// Safe for concurrent use. Uses sync.Map for endpoint lookup and
// atomic operations for the entry list.
type LatencyTracker struct {
	mu                 sync.RWMutex
	maxEndpoints       int
	samplesPerEndpoint int

	// Map from "METHOD /path" -> *EndpointLatency.
	entries sync.Map

	// Ordered list of keys for max-endpoint eviction (LRU by creation).
	keys []string
}

// NewLatencyTracker creates a tracker that stores up to maxEndpoints endpoints,
// each with up to samplesPerEndpoint recent samples for percentile calculation.
// Sensible defaults: 200 endpoints, 500 samples each.
func NewLatencyTracker(maxEndpoints, samplesPerEndpoint int) *LatencyTracker {
	if maxEndpoints <= 0 {
		maxEndpoints = 200
	}
	if samplesPerEndpoint <= 0 {
		samplesPerEndpoint = 500
	}
	return &LatencyTracker{
		maxEndpoints:       maxEndpoints,
		samplesPerEndpoint: samplesPerEndpoint,
	}
}

// Middleware returns an HTTP middleware that records request latency per endpoint.
func (t *LatencyTracker) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			dur := time.Since(start).Microseconds()
			t.record(r.Method, r.URL.Path, uint64(dur))
		})
	}
}

// record stores a latency for the given method and path.
func (t *LatencyTracker) record(method, path string, durUS uint64) {
	totalRequests.Add(1)
	key := method + " " + path

	// Fast path: existing entry.
	if entry, ok := t.entries.Load(key); ok {
		entry.(*EndpointLatency).record(durUS)
		return
	}

	// Slow path: create new entry (or adopt one created concurrently).
	t.mu.Lock()
	// Double-check after acquiring write lock.
	if entry, ok := t.entries.Load(key); ok {
		t.mu.Unlock()
		entry.(*EndpointLatency).record(durUS)
		return
	}

	// Evict oldest if at capacity.
	for len(t.keys) >= t.maxEndpoints {
		oldest := t.keys[0]
		t.keys = t.keys[1:]
		t.entries.Delete(oldest)
	}

	entry := &EndpointLatency{
		Method: method,
		Path:   path,
		buffer: make([]uint64, t.samplesPerEndpoint),
	}
	entry.record(durUS)
	t.entries.Store(key, entry)
	t.keys = append(t.keys, key)
	t.mu.Unlock()
}

// Slowest returns the top N slowest endpoints by p95 latency.
// Returns all tracked endpoints if N >= tracked count.
func (t *LatencyTracker) Slowest(n int) []LatencyStats {
	type pair struct {
		key string
		p95 uint64
	}
	var pairs []pair

	t.entries.Range(func(k, v any) bool {
		entry := v.(*EndpointLatency)
		stats := entry.Stats()
		pairs = append(pairs, pair{
			key: k.(string),
			p95: stats.P95US,
		})
		return true
	})

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].p95 > pairs[j].p95
	})

	if n > len(pairs) {
		n = len(pairs)
	}
	if n <= 0 {
		return nil
	}

	result := make([]LatencyStats, 0, n)
	for _, p := range pairs[:n] {
		if entry, ok := t.entries.Load(p.key); ok {
			result = append(result, entry.(*EndpointLatency).Stats())
		}
	}
	return result
}

// All returns stats for all tracked endpoints sorted by p95 descending.
func (t *LatencyTracker) All() []LatencyStats {
	return t.Slowest(1 << 30) // effectively all
}

// TrackerStats holds aggregate latency tracker statistics.
type TrackerStats struct {
	TrackedEndpoints int    `json:"tracked_endpoints"`
	MaxEndpoints     int    `json:"max_endpoints"`
	SamplesPerEp     int    `json:"samples_per_endpoint"`
	TotalRequests    uint64 `json:"total_requests"`
}

var totalRequests atomic.Uint64

// Stats returns aggregate tracker statistics.
func (t *LatencyTracker) Stats() TrackerStats {
	count := 0
	t.entries.Range(func(_, _ any) bool {
		count++
		return true
	})
	return TrackerStats{
		TrackedEndpoints: count,
		MaxEndpoints:     t.maxEndpoints,
		SamplesPerEp:     t.samplesPerEndpoint,
		TotalRequests:    totalRequests.Load(),
	}
}

func percentileIdx(n int, p float64) int {
	idx := int(float64(n) * p)
	if idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}
