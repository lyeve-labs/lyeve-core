package auth

import (
	"context"
	"errors"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// FlushProtectedBackend wraps a core.CacheBackend and makes Flush() a no-op.
// Refresh-token families can share one backend instance with the general
// cache, so a flush of that cache, from any provider, must not wipe them.
//
// All other operations (Get, Set, Delete) are passed through to the real backend
// unchanged. Optional interfaces (CASBackend, Ping, AcquireLock) are delegated
// so that RefreshTokenStore's internal type assertions keep working.
//
// When the underlying backend does not support Ping or AcquireLock, the
// wrapper provides a safe fallback (sentinel-write for Ping, no-op for
// AcquireLock). Compare-and-set has no safe fallback, so the wrapper offers
// CASBackend only when the backend does. A plain Set reported as a won
// compare-and-set would let two replicas both win.
type FlushProtectedBackend struct {
	backend core.CacheBackend
}

// flushProtectedCAS is the wrapper for a backend with compare-and-set.
type flushProtectedCAS struct {
	*FlushProtectedBackend
	cas CASBackend
}

// NewFlushProtectedBackend wraps backend so every core.CacheBackend call passes
// through except Flush, which is a no-op. The result implements CASBackend
// exactly when backend does.
func NewFlushProtectedBackend(backend core.CacheBackend) core.CacheBackend {
	w := &FlushProtectedBackend{backend: backend}
	if cas, ok := backend.(CASBackend); ok {
		return &flushProtectedCAS{FlushProtectedBackend: w, cas: cas}
	}
	return w
}

// ErrNoCompareAndSet refuses a refresh token store whose backend every
// replica shares but which cannot compare and set. Two replicas could then
// both rotate one token, and each hand out a successor.
var ErrNoCompareAndSet = errors.New("the shared cache backend has no compare-and-set")

// CheckSharedBackend refuses a backend that replicas share and that has no
// compare-and-set. A process-local backend needs none: the store's keyed
// mutex already serializes every rotation it sees.
func CheckSharedBackend(backend core.CacheBackend, shared bool) error {
	if _, ok := backend.(CASBackend); shared && !ok {
		return ErrNoCompareAndSet
	}
	return nil
}

// Backend returns the underlying (unwrapped) core.CacheBackend. Callers that need
// direct access: e.g. health probes that must exercise the real backend: can
// use this to bypass the Flush no-op.
func (b *FlushProtectedBackend) Backend() core.CacheBackend { return b.backend }

// core.CacheBackend interface

// Get retrieves the value for key from the underlying backend.
func (b *FlushProtectedBackend) Get(ctx context.Context, key string) ([]byte, error) {
	return b.backend.Get(ctx, key)
}

// Set stores value at key with ttl through the underlying backend.
func (b *FlushProtectedBackend) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return b.backend.Set(ctx, key, value, ttl)
}

// Delete removes the value at key through the underlying backend.
func (b *FlushProtectedBackend) Delete(ctx context.Context, key string) error {
	return b.backend.Delete(ctx, key)
}

// Flush is a no-op because the refresh-token store can share its backend with
// the general cache. A global cache flush would otherwise log out every
// authenticated user.
func (b *FlushProtectedBackend) Flush(_ context.Context) error {
	return nil
}

// Optional interface pass-throughs
//
// RefreshTokenStore uses type assertions against these interfaces. The
// wrapper explicitly implements each one so the assertions succeed when
// the underlying backend supports the contract, and degrade gracefully
// otherwise.

// CompareAndSet implements CASBackend (cross-replica rotation-race
// protection) by delegating to the backend's own.
func (b *flushProtectedCAS) CompareAndSet(ctx context.Context, key string, oldVal, newVal []byte, ttl time.Duration) error {
	return b.cas.CompareAndSet(ctx, key, oldVal, newVal, ttl)
}

// Ping delegates to the underlying backend's Ping method when available.
// Falls back to a sentinel SET (matching RefreshTokenStore.Ping()).
func (b *FlushProtectedBackend) Ping(ctx context.Context) error {
	if p, ok := b.backend.(interface{ Ping(context.Context) error }); ok {
		return p.Ping(ctx)
	}
	return b.backend.Set(ctx, "rt:health", []byte("1"), 30*time.Second)
}

// AcquireLock delegates distributed-lock acquisition when the underlying
// backend supports it (e.g. Redis RedLock). Returns a no-op release function
// and nil error when not supported: matching the Rotate behavior of
// proceeding with only the process-local keyed mutex.
func (b *FlushProtectedBackend) AcquireLock(ctx context.Context, key string, ttl time.Duration) (func() error, error) {
	if dl, ok := b.backend.(interface {
		AcquireLock(ctx context.Context, key string, ttl time.Duration) (func() error, error)
	}); ok {
		return dl.AcquireLock(ctx, key, ttl)
	}
	return func() error { return nil }, nil
}

// Compile-time interface satisfaction checks.
var _ core.CacheBackend = (*FlushProtectedBackend)(nil)
var _ CASBackend = (*flushProtectedCAS)(nil)
