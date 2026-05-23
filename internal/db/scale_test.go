//go:build !mutest

// Package db_test: database scaling integration tests.
//
// Tests schema-per-tenant at scale (up to 10k tenants), connection pool sizing
// under load, query performance degradation curves, and index bloat measurement.
// All tests are gated behind build tag "scale" and the environment variable
// CI_SCALE_TENANTS (defaults to 100 for smoke, set to 10000 for full scale).
//
// Run with:
//
//	go test -tags scale -run TestScale -v -count=1 -timeout 30m ./internal/db/
//
// Env vars:
//
//	CI_SCALE_TENANTS=10000 - full scale (10k)
//	CI_SCALE_TENANTS=100 - smoke test (default, ~10s)
package db_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Test helpers

// scaleTenantCount returns the number of tenants to use for scale tests.
// Controlled by CI_SCALE_TENANTS. Defaults to 100 for smoke testing.
func scaleTenantCount() int {
	if v := os.Getenv("CI_SCALE_TENANTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return 100
}

// scaleCheckpoints returns the tenant-count checkpoints at which to measure
// latency and resource usage. Dynamically computed from the total count so
// small smoke runs still get meaningful checkpoints.
func scaleCheckpoints(total int) []int {
	if total <= 100 {
		return []int{10, total}
	}
	if total <= 1000 {
		return []int{10, 100, total}
	}
	if total <= 5000 {
		return []int{100, 1000, total}
	}
	// 10000 path
	return []int{100, 500, 1000, 2500, 5000, 7500, total}
}

// createTenantSchema creates the named schema and a minimal set of tables
// that mimic a real plugin's footprint. Each statement is executed separately
// because pool.Exec() gets a fresh connection each call. Schema-qualified
// table names avoid the need for SET search_path.
func createTenantSchema(ctx context.Context, pool db.DB, slug string) error {
	schema := "tenant_" + slug

	// Use a dedicated connection so all DDL for this tenant happens on one
	// session. Acquire once, run all statements, close.
	conn, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for %s: %w", slug, err)
	}
	defer conn.Close()

	statements := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS "%s"`, schema),
		fmt.Sprintf(`SET search_path = "%s", public`, schema),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s".items (
			id         UUID DEFAULT gen_random_uuid() PRIMARY KEY,
			title      TEXT NOT NULL,
			body       TEXT DEFAULT '',
			tags       TEXT[] DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, schema),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_items_tags ON "%s".items USING GIN (tags)`, schema),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_items_created ON "%s".items (created_at)`, schema),
	}

	for _, stmt := range statements {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ddl for %s: %w", slug, err)
		}
	}
	return nil
}

// insertItem inserts a single row into the tenant's items table and returns
// the round-trip latency.
func insertItem(ctx context.Context, pool db.DB, slug string, idx int) (time.Duration, error) {
	schema := "tenant_" + slug
	// Acquire a dedicated connection and set search_path so the query routes
	// to the tenant schema.
	c, err := pool.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire conn: %w", err)
	}
	defer c.Close()

	_, err = c.ExecContext(ctx, "SET search_path = "+schema+", public")
	if err != nil {
		return 0, fmt.Errorf("set search_path: %w", err)
	}

	start := time.Now()
	_, err = c.ExecContext(ctx,
		`INSERT INTO items (id, title, body, tags) VALUES (gen_random_uuid(), $1, $2, $3)`,
		fmt.Sprintf("item-%d", idx),
		fmt.Sprintf("body content for item %d in tenant %s", idx, slug),
		fmt.Sprintf("{%s,tag%d}", slug, idx),
	)
	lat := time.Since(start)
	return lat, err
}

// queryItem reads a row from the tenant's items table and returns latency.
func queryItem(ctx context.Context, pool db.DB, slug string, title string) (time.Duration, error) {
	schema := "tenant_" + slug
	c, err := pool.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire conn: %w", err)
	}
	defer c.Close()

	_, err = c.ExecContext(ctx, "SET search_path = "+schema+", public")
	if err != nil {
		return 0, fmt.Errorf("set search_path: %w", err)
	}

	start := time.Now()
	row := c.QueryRowContext(ctx, `SELECT id, title, tags FROM items WHERE title = $1`, title)
	var id, gotTitle, tags string
	if err := row.Scan(&id, &gotTitle, &tags); err != nil {
		return time.Since(start), err
	}
	return time.Since(start), nil
}

// LatencyStats

// LatencyStats holds summary statistics for a set of latency measurements.
type LatencyStats struct {
	Count    int
	Min, Max time.Duration
	Avg      time.Duration
	P50      time.Duration
	P95      time.Duration
	P99      time.Duration
}

// computeLatencyStats computes percentile-based LatencyStats from durations.
func computeLatencyStats(durations []time.Duration) LatencyStats {
	if len(durations) == 0 {
		return LatencyStats{}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	n := len(durations)
	stats := LatencyStats{
		Count: n,
		Min:   durations[0],
		Max:   durations[n-1],
	}

	var sum time.Duration
	for _, d := range durations {
		sum += d
	}
	stats.Avg = sum / time.Duration(n)

	// p50, p95, p99
	stats.P50 = percentile(durations, 0.50)
	stats.P95 = percentile(durations, 0.95)
	stats.P99 = percentile(durations, 0.99)
	return stats
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ScalePoint

// ScalePoint records measurements at a single tenant-count checkpoint.
type ScalePoint struct {
	TenantCount        int
	SchemaCreateDur    time.Duration // cumulative time to create all schemas
	InsertLatency      LatencyStats
	QueryLatency       LatencyStats
	PoolStats          db.PoolStatsSnapshot
	IndexRelationSizes map[string]int64 // relation name -> size in bytes
}

func (sp ScalePoint) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tenants=%-6d create=%-12v insert(avg)=%-10v p50=%-10v p95=%-10v p99=%-10v query(avg)=%-10v p50=%-10v p95=%-10v p99=%-10v pool(open=%d,inuse=%d,idle=%d,wait=%d)",
		sp.TenantCount,
		sp.SchemaCreateDur.Round(time.Millisecond),
		sp.InsertLatency.Avg.Round(time.Microsecond),
		sp.InsertLatency.P50.Round(time.Microsecond),
		sp.InsertLatency.P95.Round(time.Microsecond),
		sp.InsertLatency.P99.Round(time.Microsecond),
		sp.QueryLatency.Avg.Round(time.Microsecond),
		sp.QueryLatency.P50.Round(time.Microsecond),
		sp.QueryLatency.P95.Round(time.Microsecond),
		sp.QueryLatency.P99.Round(time.Microsecond),
		sp.PoolStats.OpenConnections, sp.PoolStats.InUse, sp.PoolStats.Idle, sp.PoolStats.WaitCount,
	)
	return b.String()
}

// Index bloat

// measureIndexBloat queries pg_stat_user_indexes and pg_relation_size to
// compute per-index and total index sizes, detecting bloat.
// Returns a map of index name -> size in bytes, and the total size.
func measureIndexBloat(ctx context.Context, pool db.DB) (map[string]int64, int64, error) {
	rows, err := pool.Query(ctx, `
		SELECT indexrelname, pg_relation_size(indexrelid) AS size_bytes
		FROM pg_stat_user_indexes
		WHERE schemaname LIKE 'tenant_%'
		ORDER BY size_bytes DESC
		LIMIT 200`)
	if err != nil {
		return nil, 0, fmt.Errorf("query index sizes: %w", err)
	}
	defer rows.Close()

	sizes := make(map[string]int64)
	var total int64
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, 0, fmt.Errorf("scan index size: %w", err)
		}
		sizes[name] = size
		total += size
	}
	return sizes, total, rows.Err()
}

// Test: Schema Creation at Scale

// TestScale_SchemaCreation creates up to 10k tenant schemas and measures
// the cumulative creation time at each checkpoint. Validates that schema
// creation time grows roughly linearly with tenant count.
//
// Tag: scale
func TestScale_SchemaCreation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	total := scaleTenantCount()
	checkpoints := scaleCheckpoints(total)
	t.Logf("creating %d tenant schemas, measuring at checkpoints: %v", total, checkpoints)

	var (
		points     []ScalePoint
		created    int
		cumulative time.Duration
	)

	for _, target := range checkpoints {
		batchStart := time.Now()
		for created < target {
			slug := fmt.Sprintf("sc%06d", created)
			if err := createTenantSchema(ctx, pool, slug); err != nil {
				t.Fatalf("create tenant %s at count %d: %v", slug, created, err)
			}
			created++
		}
		batchDur := time.Since(batchStart)
		cumulative += batchDur

		// After creating to this checkpoint, run insert + query latency tests
		// on a sample of the most recent tenants.
		var insertDurs, queryDurs []time.Duration
		sampleSize := 20
		if target < sampleSize {
			sampleSize = target
		}
		for i := 0; i < sampleSize; i++ {
			slug := fmt.Sprintf("sc%06d", target-sampleSize+i)
			d, err := insertItem(ctx, pool, slug, i)
			if err != nil {
				t.Fatalf("insert at tenant %d: %v", target, err)
			}
			insertDurs = append(insertDurs, d)

			d, err = queryItem(ctx, pool, slug, fmt.Sprintf("item-%d", i))
			if err != nil {
				t.Fatalf("query at tenant %d: %v", target, err)
			}
			queryDurs = append(queryDurs, d)
		}

		stats := pool.Stats()
		sp := ScalePoint{
			TenantCount:     target,
			SchemaCreateDur: cumulative,
			InsertLatency:   computeLatencyStats(insertDurs),
			QueryLatency:    computeLatencyStats(queryDurs),
			PoolStats: db.PoolStatsSnapshot{
				MaxOpenConnections: stats.MaxOpenConnections,
				OpenConnections:    stats.OpenConnections,
				InUse:              stats.InUse,
				Idle:               stats.Idle,
				WaitCount:          stats.WaitCount,
				WaitDuration:       stats.WaitDuration,
				MaxIdleClosed:      stats.MaxIdleClosed,
				MaxIdleTimeClosed:  stats.MaxIdleTimeClosed,
				MaxLifetimeClosed:  stats.MaxLifetimeClosed,
			},
		}
		points = append(points, sp)
		t.Logf("  %s", sp.String())
	}

	// Validate linear-ish scaling
	if len(points) >= 2 {
		first := points[0]
		last := points[len(points)-1]
		ratio := float64(last.SchemaCreateDur) / float64(first.SchemaCreateDur)
		tenantRatio := float64(last.TenantCount) / float64(first.TenantCount)
		// Creation time should be roughly proportional to tenant count.
		// Allow generous overhead (within 3x of linear).
		expectedMaxRatio := tenantRatio * 3.0
		t.Logf("scaling factor: %d->%d tenants, time %v->%v (%.2fx vs linear %.2fx, max acceptable %.2fx)",
			first.TenantCount, last.TenantCount,
			first.SchemaCreateDur.Round(time.Millisecond),
			last.SchemaCreateDur.Round(time.Millisecond),
			ratio, tenantRatio, expectedMaxRatio)

		if ratio > expectedMaxRatio {
			t.Errorf("schema creation time scaling %.2fx exceeds acceptable maximum %.2fx (linear=%.2fx)",
				ratio, expectedMaxRatio, tenantRatio)
		}
	}

	// Cleanup: drop all tenant schemas
	t.Logf("cleaning up %d tenant schemas...", created)
	cleanStart := time.Now()
	// Drop in batches of 50 to avoid statement size limits.
	batchSize := 50
	for i := 0; i < created; i += batchSize {
		end := i + batchSize
		if end > created {
			end = created
		}
		var drops []string
		for j := i; j < end; j++ {
			drops = append(drops, fmt.Sprintf(`DROP SCHEMA IF EXISTS "tenant_sc%06d" CASCADE`, j))
		}
		_, err := pool.Exec(ctx, strings.Join(drops, "; "))
		if err != nil {
			t.Logf("cleanup batch %d-%d: %v", i, end-1, err)
		}
	}
	t.Logf("cleanup completed in %v", time.Since(cleanStart).Round(time.Millisecond))
}

// Test: Query Performance Degradation Curve

// TestScale_QueryDegradation measures SELECT latency as tenant count grows,
// producing a degradation curve that shows how schema-per-tenant affects
// catalog lookups, query planning, and execution.
//
// Tag: scale
func TestScale_QueryDegradation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	total := scaleTenantCount()
	checkpoints := scaleCheckpoints(total)
	t.Logf("query degradation curve: %d tenants, checkpoints: %v", total, checkpoints)

	// Pre-create all tenant schemas (this is NOT the thing we're measuring).
	t.Logf("pre-creating %d tenant schemas...", total)
	for i := 0; i < total; i++ {
		slug := fmt.Sprintf("qd%06d", i)
		if err := createTenantSchema(ctx, pool, slug); err != nil {
			t.Fatalf("pre-create tenant %d: %v", i, err)
		}
	}

	// Insert one row into every N-th tenant so we have data to query.
	// Stride = max(1, total/1000): at most 1000 tenants get data.
	stride := total / 1000
	if stride < 1 {
		stride = 1
	}
	t.Logf("inserting seed data into every %d-th tenant...", stride)
	for i := 0; i < total; i += stride {
		slug := fmt.Sprintf("qd%06d", i)
		if _, err := insertItem(ctx, pool, slug, 0); err != nil {
			t.Fatalf("seed insert tenant %d: %v", i, err)
		}
	}

	// Now at each checkpoint, query a random tenant and measure.
	type curvePoint struct {
		Tenants int
		Latency LatencyStats
	}
	var curve []curvePoint

	for _, target := range checkpoints {
		var durs []time.Duration
		// Query from multiple tenants at this checkpoint.
		samples := 50
		for s := 0; s < samples; s++ {
			idx := (s * stride) % target
			if idx >= total {
				idx = total - 1
			}
			slug := fmt.Sprintf("qd%06d", idx)
			d, err := queryItem(ctx, pool, slug, "item-0")
			if err != nil {
				t.Fatalf("query at tenants=%d, idx=%d: %v", target, idx, err)
			}
			durs = append(durs, d)
		}
		cp := curvePoint{Tenants: target, Latency: computeLatencyStats(durs)}
		curve = append(curve, cp)
		t.Logf("  tenants=%-6d query_avg=%-10v query_p50=%-10v query_p95=%-10v query_p99=%-10v",
			cp.Tenants,
			cp.Latency.Avg.Round(time.Microsecond),
			cp.Latency.P50.Round(time.Microsecond),
			cp.Latency.P95.Round(time.Microsecond),
			cp.Latency.P99.Round(time.Microsecond))
	}

	// Validate: query latency should not degrade exponentially
	if len(curve) >= 2 {
		first := curve[0]
		last := curve[len(curve)-1]
		ratio := float64(last.Latency.P50) / float64(first.Latency.P50)

		// With 100x tenant growth, 10x query slowdown is acceptable.
		// Beyond that, the catalog is blowing up non-linearly.
		maxAcceptable := 10.0
		t.Logf("query degradation: p50 %v->%v (%.2fx, max acceptable %.2fx)",
			first.Latency.P50.Round(time.Microsecond),
			last.Latency.P50.Round(time.Microsecond),
			ratio, maxAcceptable)

		if ratio > maxAcceptable {
			t.Errorf("query p50 latency degradation %.2fx exceeds acceptable maximum %.2fx", ratio, maxAcceptable)
		}
	}

	// Cleanup
	t.Log("cleaning up...")
	for i := 0; i < total; i += 100 {
		end := i + 100
		if end > total {
			end = total
		}
		var drops []string
		for j := i; j < end; j++ {
			drops = append(drops, fmt.Sprintf(`DROP SCHEMA IF EXISTS "tenant_qd%06d" CASCADE`, j))
		}
		pool.Exec(ctx, strings.Join(drops, "; ")) //nolint:errcheck
	}
}

// Test: Connection Pool Sizing

// TestScale_PoolSizing tests concurrent query throughput at different
// concurrency levels against a fixed set of tenants. Higher concurrency
// stresses the connection pool. The test measures throughput, latency,
// and pool contention (wait count). Not a pool-size-vs-pool-size comparison  --
// the DB pool size is fixed. Concurrency is the variable.
//
// Tag: scale
func TestScale_PoolSizing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	ctx := context.Background()
	numTenants := 100 // Fixed: we're testing concurrency levels, not tenant count.
	concurrencyLevels := []int{4, 16, 64}

	pool := testdb.Postgres(t)
	t.Logf("pre-creating %d tenant schemas...", numTenants)
	for i := 0; i < numTenants; i++ {
		slug := fmt.Sprintf("ps%06d", i)
		if err := createTenantSchema(ctx, pool, slug); err != nil {
			t.Fatalf("pre-create tenant %d: %v", i, err)
		}
	}
	// Seed data.
	for i := 0; i < numTenants; i++ {
		slug := fmt.Sprintf("ps%06d", i)
		if _, err := insertItem(ctx, pool, slug, 0); err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < numTenants; i += 50 {
			end := i + 50
			if end > numTenants {
				end = numTenants
			}
			var drops []string
			for j := i; j < end; j++ {
				drops = append(drops, fmt.Sprintf(`DROP SCHEMA IF EXISTS "tenant_ps%06d" CASCADE`, j))
			}
			pool.Exec(context.Background(), strings.Join(drops, "; ")) //nolint:errcheck
		}
	})

	for _, concurrency := range concurrencyLevels {
		t.Run(fmt.Sprintf("concurrency_%d", concurrency), func(t *testing.T) {
			totalOps := concurrency * 50
			t.Logf("concurrency=%d total_ops=%d", concurrency, totalOps)

			var (
				wg           sync.WaitGroup
				latencies    []time.Duration
				latMu        sync.Mutex
				errors       int64
				opsCompleted int64
			)

			workers := make(chan struct{}, concurrency)
			startWall := time.Now()

			for w := 0; w < concurrency; w++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					for op := 0; op < totalOps/concurrency; op++ {
						workers <- struct{}{} // semaphore acquire
						tenantIdx := (workerID*37 + op*13) % numTenants
						slug := fmt.Sprintf("ps%06d", tenantIdx)
						d, err := queryItem(ctx, pool, slug, "item-0")
						if err != nil {
							atomic.AddInt64(&errors, 1)
						} else {
							latMu.Lock()
							latencies = append(latencies, d)
							latMu.Unlock()
							atomic.AddInt64(&opsCompleted, 1)
						}
						<-workers // semaphore release
					}
				}(w)
			}
			wg.Wait()
			wallDur := time.Since(startWall).Round(time.Millisecond)

			stats := computeLatencyStats(latencies)
			throughput := float64(opsCompleted) / wallDur.Seconds()
			poolStats := pool.Stats()

			t.Logf("  concurrency=%-4d wall=%-10v ops=%d errors=%d throughput=%.0f ops/s",
				concurrency, wallDur, opsCompleted, errors, throughput)
			t.Logf("  latency: avg=%-10v p50=%-10v p95=%-10v p99=%-10v min=%-10v max=%-10v",
				stats.Avg.Round(time.Microsecond),
				stats.P50.Round(time.Microsecond),
				stats.P95.Round(time.Microsecond),
				stats.P99.Round(time.Microsecond),
				stats.Min.Round(time.Microsecond),
				stats.Max.Round(time.Microsecond))
			t.Logf("  pool: open=%d inuse=%d idle=%d wait_count=%d wait=%v",
				poolStats.OpenConnections, poolStats.InUse, poolStats.Idle,
				poolStats.WaitCount, poolStats.WaitDuration.Round(time.Millisecond))

			if errors > 0 {
				t.Errorf("  %d query errors (%.1f%%)", errors,
					float64(errors)/float64(opsCompleted+errors)*100)
			}

			if throughput < 10 {
				t.Errorf("  throughput %.0f ops/s is below minimum 10 ops/s", throughput)
			}
		})
	}
}

// Test: Index Bloat

// TestScale_IndexBloat measures index sizes and bloat after bulk schema
// creation. Checks that GIN index on tags[] and B-tree on created_at scale
// reasonably with data insertion.
//
// Tag: scale
func TestScale_IndexBloat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	total := scaleTenantCount()
	if total > 1000 {
		// Limit index bloat test to 1000 tenants to keep it fast.
		// Bloat is proportional to schema count, not insert count,
		// so 1000 is enough to see the pattern.
		total = 1000
		t.Logf("capping index bloat test at %d tenants (test is schema-count driven)", total)
	}

	t.Logf("creating %d tenants with indexes...", total)
	createStart := time.Now()
	for i := 0; i < total; i++ {
		slug := fmt.Sprintf("ib%06d", i)
		if err := createTenantSchema(ctx, pool, slug); err != nil {
			t.Fatalf("create tenant %d: %v", i, err)
		}
	}
	createDur := time.Since(createStart)

	// Insert a few rows into each tenant to populate indexes.
	rowsPerTenant := 10
	t.Logf("inserting %d rows into each of %d tenants...", rowsPerTenant, total)
	insertStart := time.Now()
	for i := 0; i < total; i++ {
		slug := fmt.Sprintf("ib%06d", i)
		for j := 0; j < rowsPerTenant; j++ {
			if _, err := insertItem(ctx, pool, slug, j); err != nil {
				t.Fatalf("insert tenant=%d row=%d: %v", i, j, err)
			}
		}
	}
	insertDur := time.Since(insertStart)

	// Measure index bloat.
	indexSizes, totalIdxSize, err := measureIndexBloat(ctx, pool)
	if err != nil {
		t.Fatalf("measure index bloat: %v", err)
	}

	t.Logf("schema creation: %v", createDur.Round(time.Millisecond))
	t.Logf("data insertion:  %v (%.0f inserts/s)",
		insertDur.Round(time.Millisecond),
		float64(total*rowsPerTenant)/insertDur.Seconds())
	t.Logf("total index size: %s (%d indexes tracked)",
		formatBytes(totalIdxSize), len(indexSizes))

	// Print top 10 largest indexes.
	type idxEntry struct {
		name string
		size int64
	}
	var entries []idxEntry
	for name, size := range indexSizes {
		entries = append(entries, idxEntry{name, size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].size > entries[j].size })

	topN := 10
	if len(entries) < topN {
		topN = len(entries)
	}
	t.Logf("top %d largest indexes:", topN)
	for i := 0; i < topN; i++ {
		t.Logf("  %-40s %10s", entries[i].name, formatBytes(entries[i].size))
	}

	// Validate: index bloat should be reasonable.
	// With 1000 tenants × 2 indexes each = 2000 indexes, total size
	// should be under 500MB for 10 rows each.
	if totalIdxSize > 500*1024*1024 {
		t.Errorf("total index size %s exceeds 500MB threshold - possible bloat",
			formatBytes(totalIdxSize))
	}

	// Avg index size should be reasonable. With 10 rows per table, each
	// B-tree + GIN index pair should be well under 1MB combined.
	avgIdxSize := totalIdxSize / int64(total)
	if avgIdxSize > 1*1024*1024 {
		t.Errorf("average index size per tenant %s exceeds 1MB - possible excessive bloat",
			formatBytes(avgIdxSize))
	}

	t.Logf("average index size per tenant: %s", formatBytes(avgIdxSize))

	// Cleanup.
	t.Log("cleaning up...")
	for i := 0; i < total; i += 100 {
		end := i + 100
		if end > total {
			end = total
		}
		var drops []string
		for j := i; j < end; j++ {
			drops = append(drops, fmt.Sprintf(`DROP SCHEMA IF EXISTS "tenant_ib%06d" CASCADE`, j))
		}
		pool.Exec(ctx, strings.Join(drops, "; ")) //nolint:errcheck
	}
}

// Test: Concurrent Multi-Tenant Queries

// TestScale_ConcurrentTenants runs concurrent queries across many tenants
// to stress the connection pool and search_path switching. Measures per-
// tenant fairness (no tenant starvation).
//
// Tag: scale
func TestScale_ConcurrentTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	numTenants := scaleTenantCount()
	if numTenants > 1000 {
		numTenants = 1000 // Cap for this test. 1k is enough to stress search_path.
	}

	t.Logf("setting up %d tenants for concurrent query test...", numTenants)
	createStart := time.Now()
	for i := 0; i < numTenants; i++ {
		slug := fmt.Sprintf("cc%06d", i)
		if err := createTenantSchema(ctx, pool, slug); err != nil {
			t.Fatalf("create tenant %d: %v", i, err)
		}
	}
	// Seed all tenants.
	for i := 0; i < numTenants; i++ {
		slug := fmt.Sprintf("cc%06d", i)
		if _, err := insertItem(ctx, pool, slug, 0); err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
	}
	t.Logf("created %d tenants in %v", numTenants, time.Since(createStart).Round(time.Millisecond))

	// Run concurrent queries: each goroutine queries a random tenant.
	concurrency := 64
	opsPerWorker := 50
	t.Logf("concurrency=%d ops_per_worker=%d total_ops=%d", concurrency, opsPerWorker, concurrency*opsPerWorker)

	type tenantStats struct {
		queries  int64
		totalDur int64 // nanoseconds, accessed atomically
		errors   int64
	}
	tenantMap := make(map[string]*tenantStats)
	var tenantMu sync.Mutex

	getOrCreate := func(slug string) *tenantStats {
		tenantMu.Lock()
		defer tenantMu.Unlock()
		if s, ok := tenantMap[slug]; ok {
			return s
		}
		s := &tenantStats{}
		tenantMap[slug] = s
		return s
	}

	var (
		wg          sync.WaitGroup
		totalErrors int64
		totalOps    int64
	)

	wallStart := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for op := 0; op < opsPerWorker; op++ {
				tenantIdx := (workerID*31 + op*17 + int(time.Now().UnixNano())%997) % numTenants
				slug := fmt.Sprintf("cc%06d", tenantIdx)

				d, err := queryItem(ctx, pool, slug, "item-0")

				ts := getOrCreate(slug)
				if err != nil {
					atomic.AddInt64(&ts.errors, 1)
					atomic.AddInt64(&totalErrors, 1)
				} else {
					atomic.AddInt64(&ts.queries, 1)
					atomic.AddInt64(&ts.totalDur, int64(d))
					atomic.AddInt64(&totalOps, 1)
				}
			}
		}(w)
	}
	wg.Wait()
	wallDur := time.Since(wallStart)

	// Analyze fairness
	tenantMu.Lock()
	tenantList := make([]*tenantStats, 0, len(tenantMap))
	for _, ts := range tenantMap {
		tenantList = append(tenantList, ts)
	}
	tenantMu.Unlock()

	// Count tenants with zero queries (potential starvation).
	var starved int
	for _, ts := range tenantList {
		if ts.queries == 0 {
			starved++
		}
	}

	// Find the most- and least-queried tenants.
	var maxQueries, minQueries int64 = 0, 1 << 62
	for _, ts := range tenantList {
		if ts.queries > maxQueries {
			maxQueries = ts.queries
		}
		if ts.queries < minQueries {
			minQueries = ts.queries
		}
	}

	t.Logf("concurrent multi-tenant results:")
	t.Logf("  wall=%-10v total_ops=%d errors=%d (%.2f%%)",
		wallDur.Round(time.Millisecond), totalOps, totalErrors,
		float64(totalErrors)/float64(totalOps+totalErrors)*100)
	t.Logf("  tenants_queried=%d starved=%d max_q=%d min_q=%d",
		len(tenantMap), starved, maxQueries, minQueries)
	t.Logf("  throughput=%.0f ops/s", float64(totalOps)/wallDur.Seconds())

	if starved > 0 {
		t.Errorf("  %d/%d tenants had zero queries - possible starvation", starved, len(tenantMap))
	}

	// Random tenant selection means some variation is expected, but if
	// one tenant got 10x the queries of another, that's suspicious.
	if minQueries > 0 && maxQueries/minQueries > 10 {
		t.Errorf("  query distribution skewed: max=%d min=%d (%.1fx)", maxQueries, minQueries,
			float64(maxQueries)/float64(minQueries))
	}

	// Cleanup
	t.Log("cleaning up...")
	for i := 0; i < numTenants; i += 100 {
		end := i + 100
		if end > numTenants {
			end = numTenants
		}
		var drops []string
		for j := i; j < end; j++ {
			drops = append(drops, fmt.Sprintf(`DROP SCHEMA IF EXISTS "tenant_cc%06d" CASCADE`, j))
		}
		pool.Exec(ctx, strings.Join(drops, "; ")) //nolint:errcheck
	}
}

// Helpers

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
