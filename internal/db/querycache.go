// Package db QueryCache caches serialized JSON results of read-heavy queries keyed by
// (dialect, normalized SQL, args hash). Intended for read-mostly data where
// TTL-based staleness is acceptable. Thread-safe: sync.RWMutex with atomic
// hit counters. Lazy expiration (no background goroutine). Bounded via LRU
// eviction on insert. Per-dialect namespaces keep dialect caches independent.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CachedResult holds a serialized JSON result with metadata.
type CachedResult struct {
	Data      []byte    `json:"data"`
	Dialect   string    `json:"dialect"`
	CacheKey  string    `json:"cache_key"`
	CachedAt  time.Time `json:"cached_at"`
	ExpiresAt time.Time `json:"expires_at"`
	HitCount  int64     `json:"hit_count"`
}

// QueryCache is a TTL-based cache for read query results.
// Keyed by dialect + SQL + args hash, stores serialized JSON.
type QueryCache struct {
	mu         sync.RWMutex
	entries    map[string]*cacheEntry
	maxSize    int
	defaultTTL time.Duration
	clock      func() time.Time // injectable for tests

	// Guards flights, the set of keys currently being filled. Held only for
	// map bookkeeping, never across a fetch, so one slow key cannot block
	// callers of any other key.
	flightMu sync.Mutex
	flights  map[string]*cacheFlight
}

type cacheEntry struct {
	data      []byte
	createdAt time.Time
	expiresAt time.Time
	hitCount  int64
}

// NewQueryCache creates a bounded TTL cache for query results.
// maxSize limits the number of cache entries (oldest evicted on overflow).
// defaultTTL is the expiration for each entry (e.g. 30s, 5m).
func NewQueryCache(maxSize int, defaultTTL time.Duration) *QueryCache {
	if maxSize <= 0 {
		maxSize = 1000
	}
	return &QueryCache{
		entries:    make(map[string]*cacheEntry),
		maxSize:    maxSize,
		defaultTTL: defaultTTL,
		clock:      time.Now,
	}
}

// BuildCacheKey creates a structured, deterministic cache key suitable for
// prefix-based invalidation. The key format is:
//
//	pluginName:dialect:tenantID:sqlHash:argsHash
//
// Each component is separated by ':' so that Invalidate("myplugin:") clears
// every entry of one plugin, and Invalidate("myplugin:postgres:tenant-1:")
// clears one tenant's entries on one dialect.
//
// Args are JSON-marshaled then FNV64a-hashed for deterministic collision-
// resistant keys. Nil and empty args produce the same hash.
func BuildCacheKey(pluginName, dialect, tenantID, sql string, args []any) string {
	// Build structured key: pluginName:dialect:tenantID:sqlHash:argsHash
	sqlHash := hashString(dialect + sql)
	argsHash := hashArgs(args)
	return pluginName + ":" + dialect + ":" + tenantID + ":" + sqlHash + ":" + argsHash
}

// hashString returns a 16-char hex encoding of the FNV64a hash of s.
func hashString(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return encodeHex(h.Sum64())
}

// hashArgs returns a 16-char hex encoding of the FNV64a hash of
// JSON-marshaled args. Nil and empty args produce the same hash.
func hashArgs(args []any) string {
	if len(args) == 0 {
		return encodeHex(0)
	}
	h := fnv.New64a()
	argBytes, err := json.Marshal(args)
	if err != nil {
		return encodeHex(0)
	}
	h.Write(argBytes)
	return encodeHex(h.Sum64())
}

func encodeHex(v uint64) string {
	const hex = "0123456789abcdef"
	buf := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		buf[i] = hex[v&0xf]
		v >>= 4
	}
	return string(buf)
}

// Get retrieves a cached result. Returns nil and false if not found or expired.
func (c *QueryCache) Get(key string) (*CachedResult, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	now := c.clock()
	if now.After(entry.expiresAt) {
		c.mu.RUnlock()
		// Expired: evict lazily under write lock.
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && c.clock().After(entry.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return nil, false
	}
	hits := atomic.AddInt64(&entry.hitCount, 1)
	c.mu.RUnlock()
	return &CachedResult{
		Data:      entry.data,
		CachedAt:  entry.createdAt,
		ExpiresAt: entry.expiresAt,
		HitCount:  hits,
		CacheKey:  key,
	}, true
}

// Set stores a value in the cache with the default TTL.
func (c *QueryCache) Set(key string, data []byte) {
	c.SetWithTTL(key, data, c.defaultTTL)
}

// SetWithTTL stores a value with a specific TTL.
func (c *QueryCache) SetWithTTL(key string, data []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock()
	// Evict oldest if at capacity.
	for len(c.entries) >= c.maxSize {
		c.evictOldestLocked()
	}

	c.entries[key] = &cacheEntry{
		data:      data,
		createdAt: now,
		expiresAt: now.Add(ttl),
	}
}

// Invalidate removes entries matching the given pattern prefix.
// The prefix is matched by string prefix on the cache key: typically
// used to invalidate all cache entries for a given table or schema.
// Returns the number of entries invalidated.
func (c *QueryCache) Invalidate(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for key := range c.entries {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(c.entries, key)
			count++
		}
	}
	return count
}

// InvalidateExact removes a specific cache entry by key.
func (c *QueryCache) InvalidateExact(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	if ok {
		delete(c.entries, key)
	}
	return ok
}

// Clear removes all entries.
func (c *QueryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry)
}

// Stats returns cache statistics for observability.
func (c *QueryCache) Stats() QueryCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var totalHits int64
	for _, e := range c.entries {
		totalHits += atomic.LoadInt64(&e.hitCount)
	}
	return QueryCacheStats{
		Size:      len(c.entries),
		MaxSize:   c.maxSize,
		TotalHits: totalHits,
	}
}

// QueryCacheStats is the observable state of the query cache.
type QueryCacheStats struct {
	Size      int   `json:"size"`
	MaxSize   int   `json:"max_size"`
	TotalHits int64 `json:"total_hits"`
}

// evictOldestLocked removes the entry with the earliest createdAt.
// Must be called with c.mu held.
func (c *QueryCache) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, e := range c.entries {
		if first || e.createdAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.createdAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// PurgeExpired removes all expired entries. Called periodically or manually.
func (c *QueryCache) PurgeExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	count := 0
	for key, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, key)
			count++
		}
	}
	return count
}

// cacheFlight is one in-flight fill for a single key. Concurrent callers that
// miss that key wait on done instead of each running the fetcher.
type cacheFlight struct {
	done chan struct{}
	data []byte
	err  error
}

// FetchInto is the read-through path behind Host.CachedFetch. It serves key
// from the cache when it is warm, otherwise fills it and unmarshals the bytes
// into dest. Reports whether the value came from the cache.
//
// Concurrent misses on one key run fn once and share the result, so a burst of
// N requests for one row issues one read rather than N. Misses on different
// keys never wait on each other.
//
// A fetcher error is never cached, and the flight is dropped either way, so
// the next caller starts a fresh one. A waiter whose leader failed runs fn
// itself on its own context: from here a leader that failed and a leader whose
// caller walked away are the same event, and neither is the waiter's failure.
// That costs a second round of reads on the error path only, which an uncoalesced read-through would pay on every path. A waiter also gives up on its own
// ctx.Done() without disturbing the leader or the other waiters.
func (c *QueryCache) FetchInto(ctx context.Context, key string, ttl time.Duration, dest any, fn func() ([]byte, error)) (bool, error) {
	if cached, hit := c.Get(key); hit {
		if dest == nil {
			return true, nil
		}
		err := json.Unmarshal(cached.Data, dest)
		if err == nil {
			return true, nil
		}
		slog.Error("query_cache: corrupt entry - evicting", "key", cached.CacheKey, "err", err)
		c.InvalidateExact(key)
	}

	data, err := c.fetchOnce(ctx, key, ttl, fn)
	if err != nil {
		return false, err
	}
	if dest != nil {
		if err := json.Unmarshal(data, dest); err != nil {
			return false, fmt.Errorf("cached_fetch unmarshal: %w", err)
		}
	}
	return false, nil
}

// fetchOnce fills key, letting at most one caller run fn at a time.
func (c *QueryCache) fetchOnce(ctx context.Context, key string, ttl time.Duration, fn func() ([]byte, error)) ([]byte, error) {
	c.flightMu.Lock()
	if f, ok := c.flights[key]; ok {
		c.flightMu.Unlock()
		select {
		case <-f.done:
			if f.err == nil {
				return f.data, nil
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return c.fill(key, ttl, fn)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.flights == nil {
		c.flights = make(map[string]*cacheFlight)
	}
	f := &cacheFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.flightMu.Unlock()

	f.data, f.err = c.fill(key, ttl, fn)

	c.flightMu.Lock()
	delete(c.flights, key)
	close(f.done)
	c.flightMu.Unlock()

	return f.data, f.err
}

// fill runs fn and stores its bytes. Errors are returned uncached.
func (c *QueryCache) fill(key string, ttl time.Duration, fn func() ([]byte, error)) ([]byte, error) {
	data, err := fn()
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	c.SetWithTTL(key, data, ttl)
	return data, nil
}
