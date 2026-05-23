// Package cache designed for slowly-changing data (users, permissions, tenants) where every
// API request would otherwise repeat the same database query.
//
// Pointer element types (e.g. *domain.User) are deep-copied at return time
// when T implements Cloner[T]. Callers cannot mutate cached data. Value types
// and types without Clone() are returned as shallow copies, which is safe for
// immutable values.
//
// Usage:
//
//	userCache := cache.NewCachedList[*domain.User](30 * time.Second)
//	users, err := userCache.GetOrLoad(r.Context(), userStore.List)
//	userCache.Invalidate() // on writes
package cache

import (
	"context"
	"sync"
	"time"
)

// Cloner is implemented by types that can produce an independent deep copy
// of themselves. When T implements Cloner[T], GetOrLoad calls Clone() on
// every element before returning the result slice.
type Cloner[T any] interface {
	Clone() T
}

// clonedSlice returns a deep copy of src where every element that implements
// Cloner[T] is replaced by its Clone(). Elements that do not implement the
// interface (values, strings, etc.) are passed through as-is from the
// shallow copy, which is safe for immutable types.
// Nil src is preserved as nil.
func clonedSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	out := make([]T, len(src))
	for i, v := range src {
		if c, ok := any(v).(Cloner[T]); ok {
			out[i] = c.Clone()
		} else {
			out[i] = v
		}
	}
	return out
}

// CachedList is a generic, goroutine-safe, TTL-based cache for list query
// results. Zero value is not usable: construct with NewCachedList.
type CachedList[T any] struct {
	mu      sync.RWMutex
	data    []T
	expires time.Time
	ttl     time.Duration
	clock   func() time.Time // injectable for tests
	gen     uint64           // bumped by Invalidate. See GetOrLoad
}

// NewCachedList returns a cache that holds a list of T with the given TTL.
// A zero or negative TTL defaults to 30 seconds.
func NewCachedList[T any](ttl time.Duration) *CachedList[T] {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &CachedList[T]{
		ttl:   ttl,
		clock: time.Now,
	}
}

// GetOrLoad returns the cached list if still valid, otherwise calls fn to
// fetch fresh data, caches it, and returns it. fn is never called when the
// cache is hit.
// Returned slices are deep copies: callers may safely mutate elements
// without corrupting the cache (when T implements Cloner[T]).
func (c *CachedList[T]) GetOrLoad(ctx context.Context, fn func(ctx context.Context) ([]T, error)) ([]T, error) {
	c.mu.RLock()
	if c.data != nil && c.clock().Before(c.expires) {
		out := clonedSlice(c.data)
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	// Cache miss or expired. Load OUTSIDE the lock and only publish the result
	// if no Invalidate landed while it was in flight.
	//
	// Running fn under the write lock would make Invalidate wait for the
	// in-flight query and make the result unconditionally cacheable: a reader
	// whose snapshot was taken just before a write committed could publish
	// that pre-write list and serve it for the whole TTL, so a list read right
	// after a create would not contain the new row. Recording the generation
	// before the load and dropping the result when it has moved closes that
	// window, and readers do not serialize behind one query.
	c.mu.RLock()
	startGen := c.gen
	c.mu.RUnlock()

	data, err := fn(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.gen == startGen {
		c.data = data
		c.expires = c.clock().Add(c.ttl)
	}
	out := clonedSlice(data)
	c.mu.Unlock()
	return out, nil
}

// Invalidate clears the cached data so the next GetOrLoad call fetches fresh.
func (c *CachedList[T]) Invalidate() {
	c.mu.Lock()
	c.data = nil
	c.expires = time.Time{}
	c.gen++
	c.mu.Unlock()
}
