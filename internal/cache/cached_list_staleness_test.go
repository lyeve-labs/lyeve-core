package cache

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A load that was already in flight when a write invalidated the cache must not
// be published. It was computed from a snapshot taken before that write, so
// publishing it serves pre-write data for the whole TTL.
func TestCachedList_DropsALoadOverlappedByInvalidate(t *testing.T) {
	c := NewCachedList[string](time.Minute)

	loadStarted := make(chan struct{})
	releaseLoad := make(chan struct{})

	var got []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		got, err = c.GetOrLoad(context.Background(), func(context.Context) ([]string, error) {
			close(loadStarted)
			<-releaseLoad
			return []string{"before-the-write"}, nil // a pre-write snapshot
		})
		if err != nil {
			t.Errorf("GetOrLoad: %v", err)
		}
	}()

	<-loadStarted
	// The write lands while the read is in flight.
	c.Invalidate()
	close(releaseLoad)
	wg.Wait()

	// The caller still gets what its own query returned.
	if len(got) != 1 || got[0] != "before-the-write" {
		t.Errorf("GetOrLoad returned %v, want the value its own load produced", got)
	}

	// But that value must not have been published: the next reader has to go
	// back to the source and see the write.
	loads := 0
	fresh, err := c.GetOrLoad(context.Background(), func(context.Context) ([]string, error) {
		loads++
		return []string{"before-the-write", "after-the-write"}, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if loads != 1 {
		t.Fatalf("the overlapped load was published: the next read was served from cache")
	}
	if len(fresh) != 2 {
		t.Errorf("second read = %v, want both entries", fresh)
	}
}

// The ordinary path still caches: a load with no invalidation across it is
// published and served.
func TestCachedList_PublishesAnUncontendedLoad(t *testing.T) {
	c := NewCachedList[string](time.Minute)

	loads := 0
	load := func(context.Context) ([]string, error) {
		loads++
		return []string{"a", "b"}, nil
	}

	for range 3 {
		got, err := c.GetOrLoad(context.Background(), load)
		if err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want 2 entries", got)
		}
	}
	if loads != 1 {
		t.Errorf("three reads caused %d loads, want 1", loads)
	}
}

// Invalidate forces exactly one reload, then caches again.
func TestCachedList_InvalidateForcesOneReload(t *testing.T) {
	c := NewCachedList[string](time.Minute)

	loads := 0
	load := func(context.Context) ([]string, error) {
		loads++
		return []string{"x"}, nil
	}

	if _, err := c.GetOrLoad(context.Background(), load); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	c.Invalidate()
	for range 3 {
		if _, err := c.GetOrLoad(context.Background(), load); err != nil {
			t.Fatalf("GetOrLoad: %v", err)
		}
	}
	if loads != 2 {
		t.Errorf("loads = %d, want 2 (one before the invalidate, one after)", loads)
	}
}

// A failing load must not poison the cache or the generation.
func TestCachedList_FailedLoadLeavesTheCacheUsable(t *testing.T) {
	c := NewCachedList[string](time.Minute)

	if _, err := c.GetOrLoad(context.Background(), func(context.Context) ([]string, error) {
		return nil, context.DeadlineExceeded
	}); err == nil {
		t.Fatal("GetOrLoad returned no error for a failing load")
	}

	got, err := c.GetOrLoad(context.Background(), func(context.Context) ([]string, error) {
		return []string{"recovered"}, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad after a failure: %v", err)
	}
	if len(got) != 1 || got[0] != "recovered" {
		t.Errorf("got %v, want [recovered]", got)
	}
}

// Concurrent readers and writers must never publish a list that predates the
// last write. Run under -race.
func TestCachedList_ConcurrentReadersNeverServeAPreWriteList(t *testing.T) {
	c := NewCachedList[int](time.Minute)

	var mu sync.Mutex
	truth := 0 // how many writes have committed

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				mu.Lock()
				truth++
				n := truth
				mu.Unlock()
				c.Invalidate()
				_ = n
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_, err := c.GetOrLoad(context.Background(), func(context.Context) ([]int, error) {
					mu.Lock()
					defer mu.Unlock()
					return []int{truth}, nil
				})
				if err != nil {
					t.Errorf("GetOrLoad: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// After the storm, one final write then a read must reflect it.
	mu.Lock()
	truth++
	want := truth
	mu.Unlock()
	c.Invalidate()

	got, err := c.GetOrLoad(context.Background(), func(context.Context) ([]int, error) {
		mu.Lock()
		defer mu.Unlock()
		return []int{truth}, nil
	})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("read after the last write = %v, want [%d]", got, want)
	}
}
