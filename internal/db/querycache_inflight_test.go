package db

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchInto_LeavesNoFlightBehind(t *testing.T) {
	c := NewQueryCache(8, time.Minute)
	key := BuildCacheKey("widgets", "postgres", "acme", "SELECT 1", nil)

	if _, err := c.FetchInto(context.Background(), key, time.Minute, nil,
		func() ([]byte, error) { return []byte(`{}`), nil }); err != nil {
		t.Fatalf("fill: %v", err)
	}
	c.flightMu.Lock()
	after := len(c.flights)
	c.flightMu.Unlock()
	if after != 0 {
		t.Fatalf("flights after a successful fill = %d, want 0", after)
	}

	failing := BuildCacheKey("widgets", "postgres", "acme", "SELECT 2", nil)
	if _, err := c.FetchInto(context.Background(), failing, time.Minute, nil,
		func() ([]byte, error) { return nil, errors.New("read failed") }); err == nil {
		t.Fatal("expected the fetcher error to surface")
	}
	c.flightMu.Lock()
	after = len(c.flights)
	c.flightMu.Unlock()
	if after != 0 {
		t.Fatalf("flights after a failed fill = %d, want 0", after)
	}
	if _, hit := c.Get(failing); hit {
		t.Fatal("a failed fetch must not cache anything")
	}
}

func TestFetchInto_CoalescesPerKeyNotAcrossKeys(t *testing.T) {
	c := NewQueryCache(64, time.Minute)

	const keys = 4
	const callersPerKey = 8
	var calls atomic.Int32

	start := make(chan struct{})
	var wg sync.WaitGroup
	for k := range keys {
		key := BuildCacheKey("widgets", "postgres", "tenant", "SELECT 1", []any{k})
		for range callersPerKey {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = c.FetchInto(context.Background(), key, time.Minute, nil,
					func() ([]byte, error) {
						calls.Add(1)
						time.Sleep(20 * time.Millisecond)
						return []byte(`{}`), nil
					})
			}()
		}
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != keys {
		t.Fatalf("fetcher ran %d times for %d keys, want %d", got, keys, keys)
	}
}
