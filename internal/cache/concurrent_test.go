package cache_test

import (
	"context"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
)

// TestMemoryCache_ConcurrentRaces exercises Get, Set, and Delete concurrently
// from 100 goroutines to surface data races in MemoryCache's internal locking.
func TestMemoryCache_ConcurrentRaces(t *testing.T) {
	c := cache.NewMemory[string, int](500, 0)
	ctx := context.Background()

	// Pre-populate so reads and deletes have something to hit.
	for i := 0; i < 200; i++ {
		c.Set(ctx, key(i), i, 0)
	}

	var wg sync.WaitGroup
	n := 100

	// Writers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			c.Set(ctx, key(seq%300), seq, 0)
		}(i)
	}

	// Readers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			c.Get(ctx, key(seq%300))
		}(i)
	}

	// Deleters
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			c.Delete(ctx, key(seq%300))
		}(i)
	}

	wg.Wait()
	// No assertions needed: -race catches data races. No panic = lock ordering correct.
}

// TestMemoryCache_ConcurrentSingleKey hammers one key with mixed operations
// to rule out per-key lock-ordering bugs under maximum contention.
func TestMemoryCache_ConcurrentSingleKey(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)
	ctx := context.Background()

	c.Set(ctx, "hot", 0, 0)

	var wg sync.WaitGroup
	n := 100

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Alternate between set and delete on the same key, plus reads.
			for j := 0; j < 10; j++ {
				c.Set(ctx, "hot", j, 0)
				c.Get(ctx, "hot")
				c.Delete(ctx, "hot")
			}
		}()
	}

	wg.Wait()
}

func key(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+i/26))
}
