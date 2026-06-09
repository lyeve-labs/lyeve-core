package plugintest

import (
	"context"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// cachedRead mirrors how a plugin store reaches the cache: assert the host to
// core.QueryCacheProvider, and fall back to a direct read when it does not
// satisfy it.
func cachedRead(t *testing.T, host core.Host, dest *int, fn core.CacheFetcher) bool {
	t.Helper()
	cp, ok := host.(core.QueryCacheProvider)
	if !ok {
		t.Fatal("test host does not satisfy core.QueryCacheProvider, so every plugin suite runs its cached read path as uncached")
	}
	hit, err := cp.CachedFetch(context.Background(), "demo", "acme", host.Dialect(),
		"SELECT count(*) FROM sys_demo WHERE tenant_id = $1", []any{"acme"},
		30*time.Second, dest, fn)
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	return hit
}

func TestHost_CachedFetchServesTheSecondReadFromTheCache(t *testing.T) {
	host := NewHost(t, nil)

	calls := 0
	fn := func() ([]byte, error) {
		calls++
		return []byte("7"), nil
	}

	var got int
	if hit := cachedRead(t, host, &got, fn); hit {
		t.Fatal("first read must be a miss")
	}
	if got != 7 {
		t.Fatalf("dest = %d, want 7", got)
	}

	got = 0
	if hit := cachedRead(t, host, &got, fn); !hit {
		t.Fatal("second read must be served from the cache")
	}
	if got != 7 {
		t.Fatalf("dest = %d, want 7", got)
	}
	if calls != 1 {
		t.Fatalf("fetcher ran %d times, want 1", calls)
	}
}

func TestHost_InvalidateCacheForcesTheNextReadToFetch(t *testing.T) {
	host := NewHost(t, nil)

	value := 1
	fn := func() ([]byte, error) {
		if value == 1 {
			return []byte("1"), nil
		}
		return []byte("2"), nil
	}

	var got int
	cachedRead(t, host, &got, fn)

	value = 2
	cachedRead(t, host, &got, fn)
	if got != 1 {
		t.Fatalf("dest = %d, want the cached 1", got)
	}

	cp := host.(core.QueryCacheProvider)
	if n := cp.InvalidateCache("demo:"); n != 1 {
		t.Fatalf("invalidated %d entries, want 1", n)
	}
	cachedRead(t, host, &got, fn)
	if got != 2 {
		t.Fatalf("dest = %d, want the refetched 2", got)
	}
}
