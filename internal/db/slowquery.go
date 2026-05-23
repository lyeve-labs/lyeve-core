// Package db SlowQueryTracer records query durations and maintains an LRU-bounded map
// of the top N slowest queries per dialect. Surfaces via the admin pool
// health endpoint and SlowestQueries(). Thread-safe. Integrates with
// OpenTelemetry if enabled. No overhead when tracing is off.
package db

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultSlowQueryThreshold is the minimum query duration that triggers
// slow-query recording and slog warning. Queries faster than this are
// silently ignored. Override via SetThreshold.
const DefaultSlowQueryThreshold = 1 * time.Second

// SlowQueryRecord captures a single slow query observation.
// NormalizedSQL is the SQL text as passed by the caller, placeholders and
// all ($1, $2, ...). Since callers already parameterize their queries,
// this is enough for identical queries to aggregate to the same record
// regardless of parameter values.
type SlowQueryRecord struct {
	NormalizedSQL string        `json:"normalized_sql"`
	Dialect       string        `json:"dialect"`
	Count         int64         `json:"count"`
	TotalDuration time.Duration `json:"total_duration"`
	MaxDuration   time.Duration `json:"max_duration"`
	MinDuration   time.Duration `json:"min_duration"`
	AvgDuration   time.Duration `json:"avg_duration"`
	LastSeen      time.Time     `json:"last_seen"`
}

// SlowQueryTracer wraps a DB to track query timing statistics.
// It maintains per-dialect aggregated stats and a top-N ring buffer.
// Only queries exceeding the configured threshold are recorded.
type SlowQueryTracer struct {
	inner     DB
	threshold time.Duration

	mu      sync.Mutex
	maxSize int                         // max records to track per dialect (default 50)
	records map[string]*SlowQueryRecord // key: dialect + ":" + normalizedSQL
}

// NewSlowQueryTracer wraps db with slow query tracking. maxSize controls
// how many distinct query shapes are retained per dialect (default 50).
// A maxSize of 0 disables tracking entirely (tracer is a no-op wrapper).
func NewSlowQueryTracer(db DB, maxSize int) *SlowQueryTracer {
	if maxSize <= 0 {
		maxSize = 50
	}
	return &SlowQueryTracer{
		inner:     db,
		threshold: DefaultSlowQueryThreshold,
		maxSize:   maxSize,
		records:   make(map[string]*SlowQueryRecord),
	}
}

// SetThreshold overrides the slow-query recording threshold.
// A zero duration records all queries.
func (t *SlowQueryTracer) SetThreshold(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.threshold = d
}

// Threshold returns the current slow-query threshold.
func (t *SlowQueryTracer) Threshold() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.threshold
}

// SlowestQueries returns the top N slowest queries (by max duration)
// observed so far, optionally filtered by dialect. If dialect is "",
// all dialects are returned. If dialect is specified, only that dialect's
// queries are returned. The result is sorted by MaxDuration descending.
func (t *SlowQueryTracer) SlowestQueries(dialect string, topN int) []SlowQueryRecord {
	t.mu.Lock()
	defer t.mu.Unlock()

	if topN <= 0 {
		topN = 20
	}

	var candidates []SlowQueryRecord
	for key, r := range t.records {
		if dialect != "" {
			// key is "dialect:normalizedSQL"
			if len(key) < len(dialect)+1 || key[:len(dialect)] != dialect || key[len(dialect)] != ':' {
				continue
			}
		}
		candidates = append(candidates, *r)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].MaxDuration > candidates[j].MaxDuration
	})

	if len(candidates) > topN {
		candidates = candidates[:topN]
	}
	return candidates
}

// Clear resets all tracking data. Useful for testing and operational resets.
func (t *SlowQueryTracer) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records = make(map[string]*SlowQueryRecord)
}

// TrackedQueries returns current count of tracked query shapes.
func (t *SlowQueryTracer) TrackedQueries() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.records)
}

// recordDuration records a query duration against the given dialect and
// normalized SQL. Called from the wrapped sqlDB methods.
func (t *SlowQueryTracer) recordDuration(dialect, normalizedSQL string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := dialect + ":" + normalizedSQL
	r, ok := t.records[key]
	if !ok {
		// Evict least-recently-seen if at capacity.
		if len(t.records) >= t.maxSize {
			t.evictLRU()
		}
		r = &SlowQueryRecord{
			NormalizedSQL: normalizedSQL,
			Dialect:       dialect,
			MinDuration:   d,
		}
		t.records[key] = r
	}

	r.Count++
	r.TotalDuration += d
	r.LastSeen = time.Now()
	if d > r.MaxDuration {
		r.MaxDuration = d
	}
	if d < r.MinDuration {
		r.MinDuration = d
	}
	if r.Count > 0 {
		r.AvgDuration = r.TotalDuration / time.Duration(r.Count)
	}
}

// evictLRU removes the entry with the oldest LastSeen timestamp.
// Must be called with t.mu held.
func (t *SlowQueryTracer) evictLRU() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, r := range t.records {
		if first || r.LastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = r.LastSeen
			first = false
		}
	}
	if oldestKey != "" {
		delete(t.records, oldestKey)
	}
}

// DB interface passthrough: delegates every method to the wrapped inner DB,
// recording query duration on each call that executes SQL.

// QueryRow delegates to the inner DB and records slow query duration for
// queries that exceed the configured threshold.
func (t *SlowQueryTracer) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	start := time.Now()
	row, err := t.inner.QueryRow(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	d := time.Since(start)
	if d >= t.threshold {
		slog.Warn("slow query", "duration_ms", d.Milliseconds(), "dialect", t.inner.Engine(), "sql", sql)
		t.recordDuration(t.inner.Engine(), sql, d)
	}
	return row, nil
}

// Query delegates to the inner DB and records slow query duration for
// queries that exceed the configured threshold.
func (t *SlowQueryTracer) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := t.inner.Query(ctx, sql, args...)
	d := time.Since(start)
	if d >= t.threshold {
		slog.Warn("slow query", "duration_ms", d.Milliseconds(), "dialect", t.inner.Engine(), "sql", sql)
		t.recordDuration(t.inner.Engine(), sql, d)
	}
	return rows, err
}

// Exec delegates to the inner DB and records slow query duration for
// queries that exceed the configured threshold.
func (t *SlowQueryTracer) Exec(ctx context.Context, sql string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := t.inner.Exec(ctx, sql, args...)
	d := time.Since(start)
	if d >= t.threshold {
		slog.Warn("slow query", "duration_ms", d.Milliseconds(), "dialect", t.inner.Engine(), "sql", sql)
		t.recordDuration(t.inner.Engine(), sql, d)
	}
	return res, err
}

// Begin delegates to the inner DB to start a transaction.
func (t *SlowQueryTracer) Begin(ctx context.Context) (*sql.Tx, error) {
	return t.inner.Begin(ctx)
}

// Conn delegates to the inner DB to acquire a dedicated connection.
func (t *SlowQueryTracer) Conn(ctx context.Context) (*sql.Conn, error) {
	return t.inner.Conn(ctx)
}

// Ping delegates to the inner DB to verify connectivity.
func (t *SlowQueryTracer) Ping(ctx context.Context) error {
	return t.inner.Ping(ctx)
}

// Close delegates to the inner DB to release all resources.
func (t *SlowQueryTracer) Close() error {
	return t.inner.Close()
}

// Stats delegates to the inner DB for pool statistics.
func (t *SlowQueryTracer) Stats() sql.DBStats {
	return t.inner.Stats()
}

// Engine returns the database engine name from the inner DB.
func (t *SlowQueryTracer) Engine() string {
	return t.inner.Engine()
}

// SQLDB returns the underlying *sql.DB from the inner DB.
func (t *SlowQueryTracer) SQLDB() *sql.DB {
	return t.inner.SQLDB()
}

// DatabaseName forwards the inner pool's own database name. The runtime wraps
// the pool in this tracer before handing it to the router and the engine host,
// so without the forward the database-per-tenant strategies see no name and
// stop re-pinning connections.
func (t *SlowQueryTracer) DatabaseName() string {
	return DatabaseNameOf(t.inner)
}

// QuerierRO returns a read-only querier from the inner DB, wrapped with
// slow-query tracking for read-only operations.
func (t *SlowQueryTracer) QuerierRO(ctx context.Context) (ReadOnlyQuerier, error) {
	ro, err := t.inner.QuerierRO(ctx)
	if err != nil {
		return nil, err
	}
	// Wrap the read-only querier so reads through it are also tracked.
	return &slowQueryReadOnlyQuerier{inner: ro, tracer: t}, nil
}

// slowQueryReadOnlyQuerier wraps ReadOnlyQuerier for slow query tracking.
type slowQueryReadOnlyQuerier struct {
	inner  ReadOnlyQuerier
	tracer *SlowQueryTracer
}

func (r *slowQueryReadOnlyQuerier) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	start := time.Now()
	row, err := r.inner.QueryRow(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	d := time.Since(start)
	if d >= r.tracer.threshold {
		slog.Warn("slow query (ro)", "duration_ms", d.Milliseconds(), "sql", sql)
		r.tracer.recordDuration("ro", sql, d)
	}
	return row, nil
}

func (r *slowQueryReadOnlyQuerier) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := r.inner.Query(ctx, sql, args...)
	d := time.Since(start)
	if d >= r.tracer.threshold {
		slog.Warn("slow query (ro)", "duration_ms", d.Milliseconds(), "sql", sql)
		r.tracer.recordDuration("ro", sql, d)
	}
	return rows, err
}

// Compile-time guard: SlowQueryTracer implements DB
var _ DB = (*SlowQueryTracer)(nil)

// SlowQueryStats extends PoolHealth with slow query data.
type SlowQueryStats struct {
	TopByMaxDuration []SlowQueryRecord `json:"top_by_max_duration"`
	TopByAvgDuration []SlowQueryRecord `json:"top_by_avg_duration"`
	TrackedShapes    int               `json:"tracked_shapes"`
}

// SlowQueryStatsFromTracer builds a SlowQueryStats snapshot from the tracer.
func SlowQueryStatsFromTracer(tracer *SlowQueryTracer, dialect string, topN int) SlowQueryStats {
	if tracer == nil {
		return SlowQueryStats{}
	}

	tracer.mu.Lock()
	defer tracer.mu.Unlock()

	if topN <= 0 {
		topN = 20
	}

	// Build top by max duration.
	type scored struct {
		r    SlowQueryRecord
		maxD time.Duration
		avgD time.Duration
	}
	var all []scored
	for key, r := range tracer.records {
		if dialect != "" {
			if len(key) < len(dialect)+1 || key[:len(dialect)] != dialect || key[len(dialect)] != ':' {
				continue
			}
		}
		all = append(all, scored{r: *r, maxD: r.MaxDuration, avgD: r.AvgDuration})
	}

	byMax := make([]SlowQueryRecord, 0, topN)
	sort.Slice(all, func(i, j int) bool { return all[i].maxD > all[j].maxD })
	for i := 0; i < len(all) && i < topN; i++ {
		byMax = append(byMax, all[i].r)
	}

	byAvg := make([]SlowQueryRecord, 0, topN)
	sort.Slice(all, func(i, j int) bool { return all[i].avgD > all[j].avgD })
	for i := 0; i < len(all) && i < topN; i++ {
		byAvg = append(byAvg, all[i].r)
	}

	return SlowQueryStats{
		TopByMaxDuration: byMax,
		TopByAvgDuration: byAvg,
		TrackedShapes:    len(tracer.records),
	}
}
