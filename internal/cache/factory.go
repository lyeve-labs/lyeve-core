package cache

import (
	"time"
)

// CacheConfig holds the parameters for constructing a cache instance.
// It mirrors the subset of config.Config used by the cache layer,
// avoiding a circular import between this package and config.
type CacheConfig struct {
	// Driver names the backend. "memory" is the only one, and New builds it
	// whatever the value.
	Driver string

	// TTL is the default entry time-to-live. 0 means entries never expire.
	TTL time.Duration

	// MaxEntries caps the LRU size (default 1 000).
	MaxEntries int
}

// New constructs an in-process Cache[string, V] from cfg. Every call returns
// a separate cache, so keyspace only labels it.
//
//	itemCache, err := cache.New[*domain.Content](cfg, "content:item")
//	listCache, err := cache.New[[]*domain.Content](cfg, "content:list")
func New[V any](cfg CacheConfig, keyspace string) (Cache[string, V], error) {
	return NewMemory[string, V](cfg.MaxEntries, cfg.TTL), nil
}
