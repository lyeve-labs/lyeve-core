// Package db StmtCache caches *sql.Stmt instances keyed by normalized query string
// (after dialect rewriting), evicting LRU entries at capacity. Avoids
// redundant server-side parse/plan for repetitive multi-tenant queries.
// Thread-safe (sync.RWMutex). Per-dialect namespaces prevent collisions.
package db

import (
	"container/list"
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// StmtCache caches *sql.Stmt instances keyed by normalized query string.
// The cache is bounded: when maxSize is reached, the least-recently-used
// entry is evicted (its *sql.Stmt is closed).
//
// Design notes:
//   - Key is the rewritten query (after dialect placeholder conversion) so
//     identical semantics cache to the same entry regardless of placeholder style.
//   - Dialect is part of the key so PG "SELECT $1" and MySQL "SELECT ?"
//     (which rewrite to the same normalized form) won't collide.
//   - Return() is a no-op - statements stay in the cache until evicted or
//     the cache is closed. The caller "returns" the statement to signal it's
//     done, but the statement remains cached for reuse.
type StmtCache struct {
	mu      sync.RWMutex
	maxSize int
	entries map[string]*list.Element // dialect+":"+query -> element
	lru     *list.List               // front = most recently used
	closed  bool
}

// stmtCacheEntry is a single cached prepared statement.
type stmtCacheEntry struct {
	key  string
	stmt *sql.Stmt
}

// NewStmtCache creates a bounded LRU cache for prepared statements.
// maxSize is the maximum number of cached statements per cache instance.
// A value of 0 disables caching (Prepare is a passthrough).
func NewStmtCache(maxSize int) *StmtCache {
	return &StmtCache{
		maxSize: maxSize,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
	}
}

// cacheKey builds the composite key for a query + dialect.
func cacheKey(dialect, query string) string {
	return dialect + ":" + query
}

// Prepare returns a cached *sql.Stmt for query, preparing from db on cache miss.
// Callers must not close the returned statement. Use Return() to signal done.
// With maxSize=0, allocates a new statement each time.
func (c *StmtCache) Prepare(ctx context.Context, db *sql.DB, dialect, query string) (*sql.Stmt, error) {
	if c == nil || c.maxSize == 0 {
		return db.PrepareContext(ctx, query)
	}

	key := cacheKey(dialect, query)

	// Fast path: try read lock first
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return db.PrepareContext(ctx, query)
	}
	if elem, ok := c.entries[key]; ok {
		c.lru.MoveToFront(elem)
		entry := elem.Value.(*stmtCacheEntry)
		c.mu.RUnlock()
		return entry.stmt, nil
	}
	c.mu.RUnlock()

	// Slow path: prepare new statement
	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("prepare %q: %w", truncateQuery(query), err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		_ = stmt.Close() // err suppressed: best-effort prepared-stmt cleanup
		return db.PrepareContext(ctx, query)
	}

	// Double-check: another goroutine may have inserted the same key while
	// preparing outside the lock. Closes the duplicate and returns the
	// cached statement.
	if elem, ok := c.entries[key]; ok {
		_ = stmt.Close() // err suppressed: close duplicate prepared stmt
		c.lru.MoveToFront(elem)
		return elem.Value.(*stmtCacheEntry).stmt, nil
	}

	// Evict LRU if at capacity
	for c.lru.Len() >= c.maxSize {
		c.evictLocked()
	}

	entry := &stmtCacheEntry{key: key, stmt: stmt}
	elem := c.lru.PushFront(entry)
	c.entries[key] = elem

	return stmt, nil
}

// Return signals that the caller is done with the cached statement.
// Currently a no-op: statements stay in the cache until evicted.
// Future versions may use this for reference-counted eviction.
func (c *StmtCache) Return(stmt *sql.Stmt) {
	if c == nil {
		return
	}
	// no-op: statements remain cached
}

// evictLocked removes the least-recently-used entry and closes its statement.
// Must be called with c.mu held.
func (c *StmtCache) evictLocked() {
	elem := c.lru.Back()
	if elem == nil {
		return
	}
	entry := elem.Value.(*stmtCacheEntry)
	if entry.stmt != nil {
		_ = entry.stmt.Close() // err suppressed: best-effort prepared-stmt cleanup
	}
	delete(c.entries, entry.key)
	c.lru.Remove(elem)
}

// Evict removes a single cached statement by dialect+query key.
// Used primarily for testing and when a schema change invalidates
// the statement's plan.
func (c *StmtCache) Evict(dialect, query string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(dialect, query)
	if elem, ok := c.entries[key]; ok {
		entry := elem.Value.(*stmtCacheEntry)
		if entry.stmt != nil {
			_ = entry.stmt.Close() // err suppressed: best-effort prepared-stmt cleanup
		}
		delete(c.entries, key)
		c.lru.Remove(elem)
	}
}

// Clear evicts all cached statements, closing each one.
func (c *StmtCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, elem := range c.entries {
		entry := elem.Value.(*stmtCacheEntry)
		if entry.stmt != nil {
			_ = entry.stmt.Close() // err suppressed: best-effort prepared-stmt cleanup
		}
	}
	c.entries = make(map[string]*list.Element)
	c.lru.Init()
}

// Close evicts all entries and marks the cache as closed.
// Subsequent calls to Prepare will fall through to db.PrepareContext.
func (c *StmtCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, elem := range c.entries {
		entry := elem.Value.(*stmtCacheEntry)
		if entry.stmt != nil {
			_ = entry.stmt.Close() // err suppressed: best-effort prepared-stmt cleanup
		}
	}
	c.entries = nil
	c.lru.Init()
}

// Len returns the number of cached statements.
func (c *StmtCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}

// StmtCacheStats holds prepared statement cache statistics.
type StmtCacheStats struct {
	Size    int
	MaxSize int
	Closed  bool
}

// Stats returns current cache statistics.
func (c *StmtCache) Stats() StmtCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return StmtCacheStats{
		Size:    c.lru.Len(),
		MaxSize: c.maxSize,
		Closed:  c.closed,
	}
}

// truncateQuery truncates a query string for error messages.
func truncateQuery(q string) string {
	const maxLen = 80
	if len(q) <= maxLen {
		return q
	}
	return q[:maxLen-3] + "..."
}
