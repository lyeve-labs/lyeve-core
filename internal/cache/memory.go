package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// memEntry holds a single cached value in the LRU list.
type memEntry[K comparable, V any] struct {
	key       K
	value     V
	expiresAt time.Time // zero = never expires
}

// MemoryCache is a thread-safe in-process LRU cache with optional per-entry TTL.
// When maxEntries is exceeded the least-recently-used entry is evicted.
type MemoryCache[K comparable, V any] struct {
	mu         sync.Mutex
	maxEntries int
	defaultTTL time.Duration
	ll         *list.List
	items      map[K]*list.Element
	clock      func() time.Time // injectable for tests. Defaults to time.Now
}

// NewMemory returns a MemoryCache.
// maxEntries ≤ 0 defaults to 1 000.
// defaultTTL = 0 means entries never expire unless evicted by LRU pressure.
func NewMemory[K comparable, V any](maxEntries int, defaultTTL time.Duration) *MemoryCache[K, V] {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &MemoryCache[K, V]{
		maxEntries: maxEntries,
		defaultTTL: defaultTTL,
		ll:         list.New(),
		items:      make(map[K]*list.Element),
		clock:      time.Now,
	}
}

// Get returns the cached value and true on a hit, or zero and false on miss/expiry.
func (c *MemoryCache[K, V]) Get(_ context.Context, key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*memEntry[K, V])
	if !e.expiresAt.IsZero() && c.clock().After(e.expiresAt) {
		c.ll.Remove(el)
		delete(c.items, key)
		var zero V
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.value, true
}

// Set stores value under key, evicting the LRU entry when over capacity.
func (c *MemoryCache[K, V]) Set(_ context.Context, key K, value V, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ttl == 0 {
		ttl = c.defaultTTL
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.clock().Add(ttl)
	}

	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*memEntry[K, V])
		e.value = value
		e.expiresAt = expiresAt
		return nil
	}

	el := c.ll.PushFront(&memEntry[K, V]{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = el

	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		e := oldest.Value.(*memEntry[K, V])
		delete(c.items, e.key)
		c.ll.Remove(oldest)
	}
	return nil
}

// Delete removes key. No-op when key is absent.
func (c *MemoryCache[K, V]) Delete(_ context.Context, key K) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*memEntry[K, V])
		delete(c.items, e.key)
		c.ll.Remove(el)
	}
	return nil
}

// Flush evicts all entries.
func (c *MemoryCache[K, V]) Flush(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[K]*list.Element)
	return nil
}
