package cache_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedList_GetOrLoad_Hit(t *testing.T) {
	c := cache.NewCachedList[string](time.Second)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"a", "b", "c"}, nil
	}

	// First call: cache miss, calls fn.
	out, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, out)
	assert.Equal(t, int32(1), calls.Load())

	// Second call: cache hit, fn not called.
	out, err = c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, out)
	assert.Equal(t, int32(1), calls.Load())
}

func TestCachedList_GetOrLoad_Expiry(t *testing.T) {
	c := cache.NewCachedList[string](20 * time.Millisecond)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"x"}, nil
	}

	_, _ = c.GetOrLoad(context.Background(), fetch)
	assert.Equal(t, int32(1), calls.Load())

	time.Sleep(30 * time.Millisecond)

	// Expired: fn called again.
	_, _ = c.GetOrLoad(context.Background(), fetch)
	assert.Equal(t, int32(2), calls.Load())
}

func TestCachedList_Invalidate(t *testing.T) {
	c := cache.NewCachedList[string](time.Minute)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"val"}, nil
	}

	_, _ = c.GetOrLoad(context.Background(), fetch)
	assert.Equal(t, int32(1), calls.Load())

	c.Invalidate()

	_, _ = c.GetOrLoad(context.Background(), fetch)
	assert.Equal(t, int32(2), calls.Load())
}

func TestCachedList_GetOrLoad_FetchError(t *testing.T) {
	c := cache.NewCachedList[int](time.Second)
	sentinel := errors.New("db down")

	fetch := func(ctx context.Context) ([]int, error) {
		return nil, sentinel
	}

	_, err := c.GetOrLoad(context.Background(), fetch)
	assert.ErrorIs(t, err, sentinel)

	// Error should NOT be cached: next call retries.
	fetchOK := func(ctx context.Context) ([]int, error) {
		return []int{1, 2}, nil
	}
	out, err := c.GetOrLoad(context.Background(), fetchOK)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, out)
}

func TestCachedList_GetOrLoad_ReturnsCopy(t *testing.T) {
	c := cache.NewCachedList[string](time.Second)

	fetch := func(ctx context.Context) ([]string, error) {
		return []string{"original"}, nil
	}

	out1, _ := c.GetOrLoad(context.Background(), fetch)
	out1[0] = "mutated"

	// Second call should return the original, unmutated value.
	out2, _ := c.GetOrLoad(context.Background(), fetch)
	assert.Equal(t, "original", out2[0])
}

func TestCachedList_DefaultTTL(t *testing.T) {
	// Zero TTL should default to 30s (and not panic).
	c := cache.NewCachedList[int](0)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]int, error) {
		calls.Add(1)
		return []int{42}, nil
	}

	_, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

// pointy is a pointer element that implements Cloner for deep-copy tests.
type pointy struct {
	Val int
}

func (p *pointy) Clone() *pointy {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}

func TestCachedList_DeepCopy_PointerType(t *testing.T) {
	c := cache.NewCachedList[*pointy](time.Second)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]*pointy, error) {
		calls.Add(1)
		return []*pointy{{Val: 1}, {Val: 2}}, nil
	}

	// First call: fetch and cache.
	out1, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, 1, out1[0].Val)
	assert.Equal(t, 2, out1[1].Val)

	// Mutate the returned slice.
	out1[0].Val = 99

	// Second call: should return original values (deep copy).
	out2, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load()) // cache hit
	assert.Equal(t, 1, out2[0].Val, "should be original value, not mutated")
	assert.Equal(t, 2, out2[1].Val)

	// Mutate the second copy too.
	out2[0].Val = 42

	// Third call: still original.
	out3, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, 1, out3[0].Val, "cache internals should be protected from mutation")
}

func TestCachedList_DeepCopy_EmptySlice(t *testing.T) {
	c := cache.NewCachedList[*pointy](time.Second)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]*pointy, error) {
		calls.Add(1)
		return []*pointy{}, nil
	}

	out, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.NotNil(t, out) // non-nil empty slice returned from fetch
}

func TestCachedList_DeepCopy_NilSlice(t *testing.T) {
	c := cache.NewCachedList[*pointy](time.Second)

	var calls atomic.Int32
	fetch := func(ctx context.Context) ([]*pointy, error) {
		calls.Add(1)
		return nil, nil
	}

	out, err := c.GetOrLoad(context.Background(), fetch)
	require.NoError(t, err)
	assert.Nil(t, out)
}
