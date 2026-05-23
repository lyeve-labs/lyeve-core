package db

import (
	"sync"
	"testing"
	"time"
)

func TestQueryCache_SetAndGet(t *testing.T) {
	c := NewQueryCache(10, 5*time.Second)

	key := BuildCacheKey("schema", "postgres", "t-1", "SELECT email FROM sys_users WHERE email = $1", []any{"default"})
	data := []byte(`{"name":"default","fields":[]}`)

	c.Set(key, data)

	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(got.Data) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", got.Data, data)
	}
	if got.HitCount != 1 {
		t.Errorf("hit count: got %d, want 1", got.HitCount)
	}
}

func TestQueryCache_Miss(t *testing.T) {
	c := NewQueryCache(10, 5*time.Second)
	_, ok := c.Get("nonexistent")
	if ok {
		t.Error("expected cache miss")
	}
}

func TestQueryCache_Expiry(t *testing.T) {
	now := time.Now()
	clk := &fakeClock{t: now}
	c := NewQueryCache(10, 1*time.Second)
	c.clock = clk.Now

	key := "test-key"
	c.Set(key, []byte("value"))

	// Advance past TTL.
	clk.Advance(2 * time.Second)

	_, ok := c.Get(key)
	if ok {
		t.Error("expected cache miss after expiry")
	}

	// Expired entry should be evicted.
	stats := c.Stats()
	if stats.Size != 0 {
		t.Errorf("expected 0 entries after expiry, got %d", stats.Size)
	}
}

func TestQueryCache_EvictOnCapacity(t *testing.T) {
	c := NewQueryCache(3, 10*time.Second)

	// Insert 3 entries.
	for i := 0; i < 3; i++ {
		key := string(rune('a' + i))
		c.Set(key, []byte{byte(i)})
	}
	if s := c.Stats().Size; s != 3 {
		t.Fatalf("expected 3 entries, got %d", s)
	}

	// Insert 4th: should evict oldest.
	c.Set("d", []byte{4})
	if s := c.Stats().Size; s != 3 {
		t.Errorf("expected 3 entries after eviction, got %d", s)
	}

	// Oldest ("a") should be gone.
	_, ok := c.Get("a")
	if ok {
		t.Error("oldest entry should have been evicted")
	}
}

func TestQueryCache_Invalidate(t *testing.T) {
	c := NewQueryCache(10, 60*time.Second)

	c.Set("schema:postgres:t-1:abc:def", []byte("1"))
	c.Set("schema:postgres:t-1:ghi:jkl", []byte("2"))
	c.Set("media:postgres:t-1:abc:def", []byte("3"))

	n := c.Invalidate("schema:")
	if n != 2 {
		t.Errorf("expected 2 invalidations, got %d", n)
	}

	_, ok := c.Get("schema:postgres:t-1:abc:def")
	if ok {
		t.Error("schema:postgres:t-1:abc:def should be invalidated")
	}
	_, ok = c.Get("media:postgres:t-1:abc:def")
	if !ok {
		t.Error("media:postgres:t-1:abc:def should survive")
	}
}

func TestQueryCache_InvalidateExact(t *testing.T) {
	c := NewQueryCache(10, 60*time.Second)

	c.Set("exact-key", []byte("x"))
	c.Set("exact-key-other", []byte("y"))

	if !c.InvalidateExact("exact-key") {
		t.Error("expected exact key to be found")
	}
	_, ok := c.Get("exact-key")
	if ok {
		t.Error("exact-key should be invalidated")
	}
	_, ok = c.Get("exact-key-other")
	if !ok {
		t.Error("exact-key-other should survive")
	}
}

func TestQueryCache_Clear(t *testing.T) {
	c := NewQueryCache(10, 60*time.Second)
	c.Set("a", []byte("1"))
	c.Set("b", []byte("2"))
	c.Clear()
	if s := c.Stats().Size; s != 0 {
		t.Errorf("expected 0 entries after clear, got %d", s)
	}
}

func TestQueryCache_Stats(t *testing.T) {
	c := NewQueryCache(10, 60*time.Second)
	c.Set("a", []byte("1"))
	c.Set("b", []byte("2"))
	c.Get("a")
	c.Get("a")

	stats := c.Stats()
	if stats.Size != 2 {
		t.Errorf("size: got %d, want 2", stats.Size)
	}
	if stats.MaxSize != 10 {
		t.Errorf("max_size: got %d, want 10", stats.MaxSize)
	}
	if stats.TotalHits != 2 {
		t.Errorf("total_hits: got %d, want 2", stats.TotalHits)
	}
}

func TestQueryCache_PurgeExpired(t *testing.T) {
	now := time.Now()
	clk := &fakeClock{t: now}
	c := NewQueryCache(10, 1*time.Second)
	c.clock = clk.Now

	c.Set("fresh", []byte("f"))
	c.SetWithTTL("short", []byte("s"), 100*time.Millisecond)

	clk.Advance(500 * time.Millisecond) // short expired, fresh still alive

	n := c.PurgeExpired()
	if n != 1 {
		t.Errorf("expected 1 purged, got %d", n)
	}

	_, ok := c.Get("fresh")
	if !ok {
		t.Error("fresh should survive")
	}
	_, ok = c.Get("short")
	if ok {
		t.Error("short should be purged")
	}
}

func TestBuildCacheKey_Deterministic(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	if a != b {
		t.Error("identical inputs should produce identical keys")
	}
}

func TestBuildCacheKey_StructuredFormat(t *testing.T) {
	// Key should be structured: pluginName:dialect:tenantID:sqlHash:argsHash
	key := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	// Verify it starts with plugin prefix for invalidation
	if key[:8] != "content:" {
		t.Errorf("key should start with 'content:', got %q", key[:8])
	}
}

func TestBuildCacheKey_DifferentForDifferentPlugin(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("media", "postgres", "t-1", "SELECT $1", []any{42})
	if a == b {
		t.Error("different plugins should produce different keys")
	}
}

func TestBuildCacheKey_DifferentForDifferentTenant(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("content", "postgres", "t-2", "SELECT $1", []any{42})
	if a == b {
		t.Error("different tenants should produce different keys")
	}
}

func TestBuildCacheKey_DifferentForDifferentSQL(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("content", "postgres", "t-1", "SELECT $2", []any{42})
	if a == b {
		t.Error("different SQL should produce different keys")
	}
}

func TestBuildCacheKey_DifferentForDifferentDialect(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("content", "mysql", "t-1", "SELECT $1", []any{42})
	if a == b {
		t.Error("different dialects should produce different keys")
	}
}

func TestBuildCacheKey_DifferentArgs(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{42})
	b := BuildCacheKey("content", "postgres", "t-1", "SELECT $1", []any{99})
	if a == b {
		t.Error("different args should produce different keys")
	}
}

func TestBuildCacheKey_NoArgs(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT 1", nil)
	b := BuildCacheKey("content", "postgres", "t-1", "SELECT 1", nil)
	if a != b {
		t.Error("no-arg queries should produce identical keys")
	}
}

func TestBuildCacheKey_NilVsEmptyArgs(t *testing.T) {
	a := BuildCacheKey("content", "postgres", "t-1", "SELECT 1", nil)
	b := BuildCacheKey("content", "postgres", "t-1", "SELECT 1", []any{})
	if a != b {
		t.Error("nil and empty args should produce identical keys")
	}
}

func TestBuildCacheKey_TenantIsolation(t *testing.T) {
	// Same query, different tenants -> different keys
	keyForT1 := BuildCacheKey("content", "postgres", "t-1", "SELECT id FROM entries", nil)
	keyForT2 := BuildCacheKey("content", "postgres", "t-2", "SELECT id FROM entries", nil)

	if keyForT1 == keyForT2 {
		t.Error("same query across tenants must produce different keys")
	}
}

func TestInvalidate_PrefixWithTenant(t *testing.T) {
	c := NewQueryCache(20, 60*time.Second)

	// Tenant-1 entries
	c.Set(BuildCacheKey("content", "postgres", "t-1", "SELECT id FROM entries", nil), []byte("t1-data"))

	// Tenant-2 entries (same query shape)
	c.Set(BuildCacheKey("content", "postgres", "t-2", "SELECT id FROM entries", nil), []byte("t2-data"))

	// Invalidate only tenant-1 content
	n := c.Invalidate("content:postgres:t-1:")
	if n != 1 {
		t.Errorf("expected 1 invalidation for tenant-1, got %d", n)
	}

	// Tenant-2 should survive
	if s := c.Stats().Size; s != 1 {
		t.Errorf("tenant-2 entry should survive, got %d entries", s)
	}
}

func TestQueryCache_ConcurrentGet_SameKey_NoDataRace(t *testing.T) {
	c := NewQueryCache(10, 5*time.Second)
	key := "shared-key"
	c.Set(key, []byte("value"))

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, ok := c.Get(key)
			if !ok {
				t.Error("expected cache hit under concurrency")
			}
		}()
	}
	wg.Wait()

	stats := c.Stats()
	if stats.TotalHits != int64(n) {
		t.Errorf("expected %d total hits, got %d", n, stats.TotalHits)
	}
}

// fakeClock

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
