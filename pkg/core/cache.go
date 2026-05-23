package core

import (
	"context"
	"time"
)

// CacheBackend is a non-generic byte-level cache backend. Used by plugins
// to provide alternative cache implementations (e.g., Redis) that the core's
// generic Cache[K,V] factory can delegate to.
type CacheBackend interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Flush(ctx context.Context) error
}

// CacheBackendProvider is implemented by plugins that provide a cache backend.
type CacheBackendProvider interface {
	Plugin
	CacheBackend() CacheBackend
}

// AuthBackendProvider is implemented by plugins that provide an isolated
// cache backend for auth data (refresh tokens, sessions) that is not
// reachable through the main cache manager's Flush path.
// prevents mass-logout on admin cache flush.
type AuthBackendProvider interface {
	Plugin
	AuthBackend() CacheBackend
}

// AuthCacheConsumer is implemented by a plugin holding lockout or block state
// that must outlive the process. Without a backend those counters live in a
// map, so an attacker who triggers a lockout gets it lifted by the next
// restart, and each replica behind a load balancer enforces its own share of
// the limit: ten replicas mean ten times the configured attempts.
//
// The runtime injects the auth cache backend after activation, which is the
// same isolated instance the refresh-token store uses: an admin cache flush
// cannot reach it, so flushing the cache does not lift every lockout.
//
// A plugin that implements this must tolerate the injection never arriving.
// An install may run with no cache backend at all, and the in-memory counters
// have to keep working on their own.
type AuthCacheConsumer interface {
	Plugin
	SetRateLimiterCache(cache CacheBackend)
}
