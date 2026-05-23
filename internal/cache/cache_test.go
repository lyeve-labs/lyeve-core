package cache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryCache_GetSet(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)

	err := c.Set(context.Background(), "key1", 42, 0)
	require.NoError(t, err)

	v, ok := c.Get(context.Background(), "key1")
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestMemoryCache_GetMiss(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)

	v, ok := c.Get(context.Background(), "nonexistent")
	assert.False(t, ok)
	assert.Equal(t, 0, v) // zero value
}

func TestMemoryCache_Delete(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)

	c.Set(context.Background(), "k", 1, 0)
	v, ok := c.Get(context.Background(), "k")
	assert.True(t, ok)
	assert.Equal(t, 1, v)

	err := c.Delete(context.Background(), "k")
	require.NoError(t, err)

	_, ok = c.Get(context.Background(), "k")
	assert.False(t, ok)
}

func TestMemoryCache_DeleteNonExistent(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)
	err := c.Delete(context.Background(), "missing")
	assert.NoError(t, err) // no-op
}

func TestMemoryCache_Flush(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)
	c.Set(context.Background(), "a", 1, 0)
	c.Set(context.Background(), "b", 2, 0)
	c.Set(context.Background(), "c", 3, 0)

	err := c.Flush(context.Background())
	require.NoError(t, err)

	for _, k := range []string{"a", "b", "c"} {
		_, ok := c.Get(context.Background(), k)
		assert.False(t, ok, "key %s should be flushed", k)
	}
}

func TestMemoryCache_UpdateExisting(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)

	c.Set(context.Background(), "x", 10, 0)
	c.Set(context.Background(), "x", 20, 0)

	v, ok := c.Get(context.Background(), "x")
	assert.True(t, ok)
	assert.Equal(t, 20, v)
}

func TestMemoryCache_DefaultMaxEntries(t *testing.T) {
	// maxEntries=0 should default to 1000
	c := cache.NewMemory[string, int](0, 0)
	for i := 0; i < 100; i++ {
		err := c.Set(context.Background(), string(rune('a'+i%26))+string(rune('0'+i/26)), i, 0)
		require.NoError(t, err)
	}
	// all entries should be present (far below 1000)
	for i := 0; i < 100; i++ {
		k := string(rune('a'+i%26)) + string(rune('0'+i/26))
		_, ok := c.Get(context.Background(), k)
		assert.True(t, ok, "key %s should be present", k)
	}
}

func TestMemoryCache_Eviction(t *testing.T) {
	// Tiny cache that can hold only 3 items
	c := cache.NewMemory[string, int](3, 0)

	c.Set(context.Background(), "a", 1, 0)
	c.Set(context.Background(), "b", 2, 0)
	c.Set(context.Background(), "c", 3, 0)
	// LRU: c is newest, a is oldest
	c.Set(context.Background(), "d", 4, 0) // should evict "a"

	_, ok := c.Get(context.Background(), "a")
	assert.False(t, ok, "a should be evicted")
	assertGet(t, c, "b", 2)
	assertGet(t, c, "c", 3)
	assertGet(t, c, "d", 4)
}

func TestMemoryCache_LRUOrder_GetPromotes(t *testing.T) {
	c := cache.NewMemory[string, int](2, 0)

	c.Set(context.Background(), "a", 1, 0)
	c.Set(context.Background(), "b", 2, 0)
	// LRU: b newest, a oldest

	// Access "a" - should move to front
	c.Get(context.Background(), "a")

	// Now insert "c" - should evict "b" (not "a")
	c.Set(context.Background(), "c", 3, 0)

	assertGet(t, c, "a", 1)
	_, ok := c.Get(context.Background(), "b")
	assert.False(t, ok, "b should be evicted after a was promoted")
	assertGet(t, c, "c", 3)
}

func TestMemoryCache_TTL(t *testing.T) {
	c := cache.NewMemory[string, int](100, 50*time.Millisecond)

	c.Set(context.Background(), "short", 1, 50*time.Millisecond)

	// Immediately should be present
	v, ok := c.Get(context.Background(), "short")
	assert.True(t, ok)
	assert.Equal(t, 1, v)

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	v, ok = c.Get(context.Background(), "short")
	assert.False(t, ok, "entry should have expired")
	assert.Equal(t, 0, v)
}

func TestMemoryCache_TTL_DefaultTTL(t *testing.T) {
	c := cache.NewMemory[string, int](100, 30*time.Millisecond)

	// Set with ttl=0 uses defaultTTL
	c.Set(context.Background(), "defval", 99, 0)

	v, ok := c.Get(context.Background(), "defval")
	assert.True(t, ok)
	assert.Equal(t, 99, v)

	time.Sleep(60 * time.Millisecond)
	_, ok = c.Get(context.Background(), "defval")
	assert.False(t, ok)
}

func TestMemoryCache_TTL_NoExpiry(t *testing.T) {
	c := cache.NewMemory[string, int](100, 0)
	c.Set(context.Background(), "forever", 42, 0)

	time.Sleep(10 * time.Millisecond)

	v, ok := c.Get(context.Background(), "forever")
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestMemoryCache_TTL_PerEntryOverride(t *testing.T) {
	c := cache.NewMemory[string, int](100, 50*time.Millisecond)

	// Per-entry TTL overrides default
	c.Set(context.Background(), "fast", 1, 10*time.Millisecond)
	c.Set(context.Background(), "slow", 2, 500*time.Millisecond)

	time.Sleep(30 * time.Millisecond)

	_, ok := c.Get(context.Background(), "fast")
	assert.False(t, ok, "fast should have expired")

	v, ok := c.Get(context.Background(), "slow")
	assert.True(t, ok, "slow should still be alive")
	assert.Equal(t, 2, v)
}

func TestMemoryCache_ConcurrentAccess(t *testing.T) {
	c := cache.NewMemory[string, int](1000, 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	n := 50

	// Concurrent writes
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set(ctx, string(rune('a'+i%26))+string(rune('0'+i)), i, 0)
		}(i)
	}
	wg.Wait()

	// Concurrent reads
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := string(rune('a'+i%26)) + string(rune('0'+i))
			v, ok := c.Get(ctx, k)
			assert.True(t, ok)
			assert.Equal(t, i, v)
		}(i)
	}
	wg.Wait()
}

func TestMemoryCache_ConcurrentSetGet(t *testing.T) {
	c := cache.NewMemory[string, int](1000, 0)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.Set(ctx, "shared", i, 0)
		}(i)
		go func() {
			defer wg.Done()
			c.Get(ctx, "shared")
		}()
	}
	wg.Wait()
	// no panic = pass
}

func TestNew_MemoryDriver(t *testing.T) {
	cfg := cache.CacheConfig{
		Driver:     "memory",
		MaxEntries: 50,
		TTL:        10 * time.Second,
	}
	c, err := cache.New[int](cfg, "testspace")
	require.NoError(t, err)
	require.NotNil(t, c)

	err = c.Set(context.Background(), "k", 42, 0)
	require.NoError(t, err)
	v, ok := c.Get(context.Background(), "k")
	assert.True(t, ok)
	assert.Equal(t, 42, v)
}

func TestNew_DefaultDriver(t *testing.T) {
	cfg := cache.CacheConfig{} // Driver empty
	c, err := cache.New[string](cfg, "ns")
	require.NoError(t, err)
	require.NotNil(t, c)
}

func assertGet(t *testing.T, c cache.Cache[string, int], key string, expected int) {
	t.Helper()
	v, ok := c.Get(context.Background(), key)
	assert.True(t, ok, "key %s should exist", key)
	assert.Equal(t, expected, v)
}
