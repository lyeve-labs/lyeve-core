// Package cache provides a generic, goroutine-safe read-through cache for
// reducing database load on read-heavy content queries.
//
// Built-in implementation:
//   - MemoryCache - in-process LRU + TTL (default, no extra dependencies)
//
// Use New[V]() to construct a typed memory cache from a CacheConfig:
//
//	c, err := cache.New[*domain.Content](cache.CacheConfig{TTL: 60 * time.Second}, "content:item")
package cache

import (
	"context"
	"time"
)

// Cache is a generic, goroutine-safe key-value cache.
// K must be comparable. V may be any JSON-serializable type.
type Cache[K comparable, V any] interface {
	// Get returns the cached value and true on a hit,
	// or the zero value and false on a miss or expired entry.
	Get(ctx context.Context, key K) (V, bool)

	// Set stores value under key.
	// A zero ttl uses the cache's configured default (no expiry if default is also zero).
	Set(ctx context.Context, key K, value V, ttl time.Duration) error

	// Delete removes key. No-op when key does not exist.
	Delete(ctx context.Context, key K) error

	// Flush removes all entries.
	Flush(ctx context.Context) error
}
