package enginehost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// cachedHost returns a host with a query cache wired and nothing else. Every
// test here exercises CachedFetch, which touches no pool.
func cachedHost() *engineHost {
	h := &engineHost{}
	h.WithQueryCache(db.NewQueryCache(64, time.Minute))
	return h
}

// fetch calls CachedFetch with the argument shape a plugin store uses.
func fetch(ctx context.Context, h *engineHost, key string, dest any, fn func() ([]byte, error)) (bool, error) {
	return h.CachedFetch(ctx, "widgets", "acme", "postgres",
		"SELECT item_limit FROM widgets WHERE tenant_id = $1", []any{key},
		10*time.Second, dest, fn)
}

func TestCachedFetch_ConcurrentMissesRunTheFetcherOnce(t *testing.T) {
	h := cachedHost()

	var calls atomic.Int32
	fn := func() ([]byte, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return json.Marshal(map[string]int{"limit": 10})
	}

	const callers = 25
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]map[string]int, callers)
	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var got map[string]int
			_, errs[i] = fetch(context.Background(), h, "acme", &got, fn)
			results[i] = got
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), calls.Load(), "one read for one key")
	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, map[string]int{"limit": 10}, results[i], "every caller gets the result")
	}
}

func TestCachedFetch_DistinctKeysDoNotBlockEachOther(t *testing.T) {
	h := cachedHost()

	release := make(chan struct{})
	slow := func() ([]byte, error) {
		<-release
		return []byte(`{"limit":1}`), nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var got map[string]int
		_, _ = fetch(context.Background(), h, "slow-tenant", &got, slow)
	}()

	var got map[string]int
	_, err := fetch(context.Background(), h, "other-tenant", &got,
		func() ([]byte, error) { return []byte(`{"limit":2}`), nil })
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"limit": 2}, got, "a second key fills while the first is still in flight")

	close(release)
	<-done
}

func TestCachedFetch_WarmKeyDoesNotRefetch(t *testing.T) {
	h := cachedHost()

	var calls atomic.Int32
	fn := func() ([]byte, error) {
		calls.Add(1)
		return []byte(`{"limit":7}`), nil
	}

	var first map[string]int
	hit, err := fetch(context.Background(), h, "acme", &first, fn)
	require.NoError(t, err)
	assert.False(t, hit, "first call fills the entry")

	var second map[string]int
	hit, err = fetch(context.Background(), h, "acme", &second, fn)
	require.NoError(t, err)
	assert.True(t, hit, "second call is served from the cache")
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, first, second)
}

func TestCachedFetch_FetcherErrorIsNotCached(t *testing.T) {
	h := cachedHost()

	boom := errors.New("connection refused")
	var got map[string]int
	_, err := fetch(context.Background(), h, "acme", &got, func() ([]byte, error) { return nil, boom })
	require.ErrorIs(t, err, boom)

	// The failed key is neither cached nor left in flight: the next caller
	// runs its own fetcher and succeeds.
	var calls atomic.Int32
	hit, err := fetch(context.Background(), h, "acme", &got, func() ([]byte, error) {
		calls.Add(1)
		return []byte(`{"limit":3}`), nil
	})
	require.NoError(t, err)
	assert.False(t, hit)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, map[string]int{"limit": 3}, got)
}

func TestCachedFetch_WaiterRetriesWhenTheLeaderFails(t *testing.T) {
	h := cachedHost()

	leaderEntered := make(chan struct{})
	var calls atomic.Int32
	fn := func() ([]byte, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			time.Sleep(30 * time.Millisecond)
			return nil, errors.New("leader read failed")
		}
		return []byte(`{"limit":5}`), nil
	}

	leaderErr := make(chan error, 1)
	go func() {
		var ignored map[string]int
		_, err := fetch(context.Background(), h, "acme", &ignored, fn)
		leaderErr <- err
	}()

	<-leaderEntered
	var got map[string]int
	_, err := fetch(context.Background(), h, "acme", &got, fn)
	require.NoError(t, err, "the waiter does not inherit the leader's failure")
	assert.Equal(t, map[string]int{"limit": 5}, got)
	assert.Error(t, <-leaderErr, "the leader still sees its own failure")
}

func TestCachedFetch_LeaderCancellationDoesNotFailWaiters(t *testing.T) {
	h := cachedHost()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderEntered := make(chan struct{})
	var calls atomic.Int32
	fn := func() ([]byte, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			<-leaderCtx.Done()
			return nil, leaderCtx.Err()
		}
		return []byte(`{"limit":9}`), nil
	}

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		var ignored map[string]int
		_, _ = fetch(leaderCtx, h, "acme", &ignored, fn)
	}()

	<-leaderEntered
	waiterDone := make(chan error, 1)
	var got map[string]int
	go func() {
		_, err := fetch(context.Background(), h, "acme", &got, fn)
		waiterDone <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	<-leaderDone

	require.NoError(t, <-waiterDone, "the leader's cancellation is not the waiter's failure")
	assert.Equal(t, map[string]int{"limit": 9}, got)
}

func TestCachedFetch_WaiterHonorsItsOwnDeadline(t *testing.T) {
	h := cachedHost()

	release := make(chan struct{})
	leaderEntered := make(chan struct{})
	var calls atomic.Int32
	fn := func() ([]byte, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			<-release
		}
		return []byte(`{"limit":4}`), nil
	}

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		var ignored map[string]int
		_, _ = fetch(context.Background(), h, "acme", &ignored, fn)
	}()

	<-leaderEntered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var got map[string]int
	_, err := fetch(ctx, h, "acme", &got, fn)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a waiter gives up on its own deadline")

	close(release)
	<-leaderDone
	assert.Equal(t, int32(1), calls.Load(), "the abandoned waiter never ran the fetcher")
}

func TestCachedFetch_WithoutACacheStillFillsDest(t *testing.T) {
	h := &engineHost{}

	var got map[string]int
	hit, err := h.CachedFetch(context.Background(), "widgets", "acme", "postgres", "SELECT 1", nil,
		time.Second, &got, func() ([]byte, error) { return []byte(`{"limit":2}`), nil })
	require.NoError(t, err)
	assert.False(t, hit)
	assert.Equal(t, map[string]int{"limit": 2}, got)
}
