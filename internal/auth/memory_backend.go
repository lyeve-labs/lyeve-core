package auth

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"
)

// MemoryBackend is an in-process CacheBackend used as the refresh-token store
// fallback when no plugin provides a core.CacheBackend. It keeps Redis
// entirely out of core: single-instance deployments and tests get working
// refresh-token rotation without any external dependency, while distributed
// deployments receive the plugin-provided backend instead.
//
// It is intentionally minimal: a map guarded by a mutex with lazy TTL expiry.
// Refresh-token volume is low and entries self-expire, so no LRU/eviction is
// needed. Entries that expire without ever being read are reclaimed by an
// opportunistic sweep on write.
type MemoryBackend struct {
	mu    sync.Mutex
	items map[string]memItem
}

type memItem struct {
	val       []byte
	expiresAt time.Time // zero = no expiry
}

// errMemMiss signals a key that is absent or expired. Callers treat any Get
// error as a miss, so the concrete error is an internal detail.
var errMemMiss = errors.New("auth: memory backend: key not found")

// sweepThreshold is the map size above which a write triggers an expiry sweep.
const sweepThreshold = 256

// NewMemoryBackend returns an empty in-process cache backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{items: make(map[string]memItem)}
}

// Get returns the value for key, or errMemMiss when the key is absent or expired.
func (b *MemoryBackend) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	it, ok := b.items[key]
	if !ok {
		return nil, errMemMiss
	}
	if !it.expiresAt.IsZero() && time.Now().After(it.expiresAt) {
		delete(b.items, key)
		return nil, errMemMiss
	}
	// Return a defensive copy so callers cannot mutate stored bytes.
	out := make([]byte, len(it.val))
	copy(out, it.val)
	return out, nil
}

// Set stores value at key with ttl. Opportunistically sweeps expired entries
// when the map exceeds sweepThreshold.
func (b *MemoryBackend) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	stored := make([]byte, len(value))
	copy(stored, value)
	b.items[key] = memItem{val: stored, expiresAt: exp}

	if len(b.items) > sweepThreshold {
		b.sweepExpiredLocked()
	}
	return nil
}

// Delete removes the value at key from the store.
func (b *MemoryBackend) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.items, key)
	return nil
}

// Flush removes all entries from the store.
func (b *MemoryBackend) Flush(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = make(map[string]memItem)
	return nil
}

// CompareAndSet implements CASBackend. It atomically replaces the value at key
// with newVal only when the current value matches oldVal. Returns ErrCASFailed
// if the key doesn't exist (and oldVal is non-empty) or the values don't match.
func (b *MemoryBackend) CompareAndSet(_ context.Context, key string, oldVal, newVal []byte, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	it, ok := b.items[key]
	if !ok {
		// Absent key: fail unless the caller expects create-if-absent (empty oldVal).
		if len(oldVal) > 0 {
			return ErrCASFailed
		}
	} else {
		if !it.expiresAt.IsZero() && time.Now().After(it.expiresAt) {
			// Expired reads as absent, so the same create-if-absent rule applies.
			delete(b.items, key)
			if len(oldVal) > 0 {
				return ErrCASFailed
			}
		} else if !bytes.Equal(it.val, oldVal) {
			return ErrCASFailed
		}
	}

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	stored := make([]byte, len(newVal))
	copy(stored, newVal)
	b.items[key] = memItem{val: stored, expiresAt: exp}
	return nil
}

// sweepExpiredLocked removes expired entries. The caller must hold b.mu.
func (b *MemoryBackend) sweepExpiredLocked() {
	now := time.Now()
	for k, it := range b.items {
		if !it.expiresAt.IsZero() && now.After(it.expiresAt) {
			delete(b.items, k)
		}
	}
}
