package middleware

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBucketLRU_EvictsTheLeastRecentlyUsed(t *testing.T) {
	c := newBucketLRU(3)
	for _, k := range []string{"a", "b", "c"} {
		c.add(k, &tokenBucket{})
	}
	_, _ = c.get("a") // a is now the most recent. B is the oldest
	c.add("d", &tokenBucket{})

	assert.Equal(t, 3, c.len())
	assert.False(t, c.has("b"), "the least recently used bucket goes first")
	for _, k := range []string{"a", "c", "d"} {
		assert.True(t, c.has(k), k)
	}
}

func TestBucketLRU_RemoveIf(t *testing.T) {
	c := newBucketLRU(0)
	stale := &tokenBucket{tokens: -1}
	c.add("stale", stale)
	c.add("live", &tokenBucket{})
	c.removeIf(func(b *tokenBucket) bool { return b == stale })
	assert.False(t, c.has("stale"))
	assert.True(t, c.has("live"))
}

// A caller presenting a new key on every request, once the cap is reached,
// must not make each request scan every bucket under the limiter's lock.
func BenchmarkBucketLRU_NewKeyAtCapacity(b *testing.B) {
	c := newBucketLRU(10_000)
	for i := 0; i < 10_000; i++ {
		c.add(fmt.Sprintf("warm-%d", i), &tokenBucket{})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.add(fmt.Sprintf("new-%d", i), &tokenBucket{})
	}
}
