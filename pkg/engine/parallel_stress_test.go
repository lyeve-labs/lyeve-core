package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParallelEngine_FanOut_10000Items fans out 10,000 items with 100
// concurrent workers and verifies every result is correct.
func TestParallelEngine_FanOut_10000Items(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 100, Timeout: 30 * time.Second})

	const itemCount = 10_000
	items := make([]any, itemCount)
	for i := range items {
		items[i] = i
	}

	results, err := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		n := item.(int)
		return n * 2, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != itemCount {
		t.Fatalf("expected %d results, got %d", itemCount, len(results))
	}

	// Verify every expected value is present. Results arrive in arbitrary
	// order, so index by value.
	seen := make(map[int]bool, itemCount)
	for _, r := range results {
		seen[r.Value.(int)] = true
	}
	for i := 0; i < itemCount; i++ {
		expected := i * 2
		if !seen[expected] {
			t.Errorf("missing result for input %d (expected value %d)", i, expected)
		}
	}
}

// TestParallelEngine_FanOutVoid_Concurrent runs 50 concurrent callers each
// calling FanOutVoid on the same engine, verifying no races and all items
// are processed.
func TestParallelEngine_FanOutVoid_Concurrent(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 16, Timeout: 30 * time.Second})

	const (
		callers  = 50
		perBatch = 100
	)

	var totalProcessed atomic.Int64

	var wg sync.WaitGroup
	for caller := 0; caller < callers; caller++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items := make([]any, perBatch)
			for i := range items {
				items[i] = i
			}
			engine.FanOutVoid(context.Background(), items, func(ctx context.Context, item any, idx int) error {
				totalProcessed.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()

	expected := int64(callers * perBatch)
	if n := totalProcessed.Load(); n != expected {
		t.Errorf("expected %d items processed, got %d", expected, n)
	}
}

// TestParallelEngine_Timeout sets a 10ms per-task timeout and submits tasks
// that take 1s. Tasks that respect the deadline should report
// context.DeadlineExceeded.
func TestParallelEngine_Timeout(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 8, Timeout: 10 * time.Millisecond})

	items := make([]any, 20)
	for i := range items {
		items[i] = i
	}

	results, firstErr := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		// Respect the deadline: the engine-provided context has already
		// been wrapped with a 10ms timeout. This select aborts when the
		// deadline fires, long before the 1s sleep finishes.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
			return item, nil
		}
	})

	if firstErr == nil {
		t.Error("expected at least one timeout error, got nil")
	}

	timeoutCount := 0
	for _, r := range results {
		if r.Err != nil {
			timeoutCount++
		}
	}
	if timeoutCount == 0 {
		t.Error("expected at least one result to have a timeout error")
	}
	// With 8 concurrent slots running 1s tasks and 10ms timeout, every
	// task should time out. Confirm we got results for all items.
	if len(results) != 20 {
		t.Errorf("expected 20 results, got %d", len(results))
	}
}

// TestParallelEngine_Timeout_NoDeadline checks that when Timeout is zero,
// tasks are not subject to a per-task deadline.
func TestParallelEngine_Timeout_NoDeadline(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4, Timeout: 0})

	items := make([]any, 10)
	for i := range items {
		items[i] = i
	}

	results, err := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		// With no timeout, the context should not have a deadline.
		deadline, ok := ctx.Deadline()
		if ok {
			return nil, fmt.Errorf("unexpected deadline: %v", deadline)
		}
		return item, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 10 {
		t.Errorf("expected 10 results, got %d", len(results))
	}
}

// TestParallelEngine_ContextCancel cancels the parent context mid-flight and
// verifies that remaining tasks abort.
func TestParallelEngine_ContextCancel(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4})

	// Use enough items and a small enough concurrency that many tasks
	// are still queued when we cancel.
	const itemCount = 200
	items := make([]any, itemCount)
	for i := range items {
		items[i] = i
	}

	var started atomic.Int64
	var completed atomic.Int64
	var canceled atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())

	// Start FanOut in a separate goroutine so we can cancel mid-flight.
	var results []FanOutResult[any]
	var fanErr error
	done := make(chan struct{})

	go func() {
		results, fanErr = engine.FanOut(ctx, items, func(taskCtx context.Context, item any, idx int) (any, error) {
			started.Add(1)

			// The task respects cancellation: wait for either the
			// deadline to pass or context to be canceled.
			select {
			case <-taskCtx.Done():
				canceled.Add(1)
				return nil, taskCtx.Err()
			case <-time.After(200 * time.Millisecond):
				completed.Add(1)
				return item, nil
			}
		})
		close(done)
	}()

	// Cancel after a short delay: some tasks will have started but
	// the bulk should still be queued.
	time.Sleep(30 * time.Millisecond)
	cancel()

	// Wait for FanOut to return.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("FanOut did not return after context cancel")
	}

	// After cancellation, FanOut returns results for whatever finished
	// plus whatever errored due to cancellation. The first error should
	// be context.Canceled.
	if fanErr == nil {
		t.Error("expected an error from context cancellation, got nil")
	}
	if len(results) == 0 {
		t.Error("expected at least some results, got none")
	}

	startedCount := started.Load()
	completedCount := completed.Load()
	canceledCount := canceled.Load()

	// Every started task should either complete or cancel.
	if startedCount != completedCount+canceledCount {
		t.Errorf("started=%d does not match completed=%d + canceled=%d",
			startedCount, completedCount, canceledCount)
	}

	// Not all items should have completed (cancellation aborted the rest).
	if completedCount == int64(itemCount) {
		t.Error("all items completed; expected cancellation to abort some")
	}

	t.Logf("started=%d completed=%d canceled=%d", startedCount, completedCount, canceledCount)
}

// TestParallelEngine_ErrorIsolation verifies that when one task errors,
// other tasks still complete successfully and return correct values.
func TestParallelEngine_ErrorIsolation(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 16})

	const itemCount = 500
	items := make([]any, itemCount)
	for i := range items {
		items[i] = i
	}

	taskErr := errors.New("intentional task failure")

	results, firstErr := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		n := item.(int)
		// Every 7th item errors.
		if n%7 == 0 {
			return nil, fmt.Errorf("item %d: %w", n, taskErr)
		}
		return n * 3, nil
	})

	if firstErr == nil {
		t.Error("expected at least one error, got nil")
	}
	if len(results) != itemCount {
		t.Fatalf("expected %d results, got %d", itemCount, len(results))
	}

	errorCount := 0
	successValues := make(map[int]bool)

	for _, r := range results {
		if r.Err != nil {
			errorCount++
		} else {
			successValues[r.Value.(int)] = true
		}
	}

	// Every 7th item: floor(500/7) = 71 errors from items 0, 7, 14, ...
	expectedErrors := 0
	for i := 0; i < itemCount; i++ {
		if i%7 == 0 {
			expectedErrors++
		}
	}
	if errorCount != expectedErrors {
		t.Errorf("expected %d errors, got %d", expectedErrors, errorCount)
	}

	// Verify all successful items produced correct values.
	for i := 0; i < itemCount; i++ {
		if i%7 == 0 {
			continue // this item errored
		}
		expected := i * 3
		if !successValues[expected] {
			t.Errorf("missing success value %d (from item %d)", expected, i)
		}
	}
}

// TestParallelEngine_SequentialFallback_Strict verifies that when
// MaxConcurrent=1, tasks execute strictly one at a time. This is a more
// rigorous version of TestParallelEngine_SequentialFallback in
// parallel_test.go.
func TestParallelEngine_SequentialFallback_Strict(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 1})

	items := make([]any, 50)
	for i := range items {
		items[i] = i
	}

	var inFlight atomic.Int64
	var maxObserved atomic.Int64

	engine.FanOutVoid(context.Background(), items, func(ctx context.Context, item any, idx int) error {
		current := inFlight.Add(1)

		// Track the maximum concurrency observed.
		for {
			old := maxObserved.Load()
			if current <= old || maxObserved.CompareAndSwap(old, current) {
				break
			}
		}

		// Tiny sleep to give any would-be concurrent goroutine time to
		// also enter the critical section if the semaphore were broken.
		time.Sleep(time.Millisecond)

		inFlight.Add(-1)
		return nil
	})

	if max := maxObserved.Load(); max > 1 {
		t.Errorf("MaxConcurrent=1 but observed %d tasks in flight concurrently", max)
	}
}

// TestParallelEngine_LargeFanOut fans out 50,000 items and verifies
// correctness while confirming the engine handles large batches without
// unbounded memory growth.
func TestParallelEngine_LargeFanOut(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 32, Timeout: 60 * time.Second})

	const itemCount = 50_000
	items := make([]any, itemCount)
	for i := range items {
		items[i] = i
	}

	// Capture heap before.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	results, err := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		n := item.(int)
		return n * 2, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != itemCount {
		t.Fatalf("expected %d results, got %d", itemCount, len(results))
	}

	// Capture heap after.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Verify all expected values.
	seen := make(map[int]bool, itemCount)
	for _, r := range results {
		seen[r.Value.(int)] = true
	}
	for i := 0; i < itemCount; i++ {
		if !seen[i*2] {
			t.Errorf("missing result for input %d", i)
		}
	}

	// Heap growth should be reasonable (results slice of 50k structs is
	// ~50k * 40 bytes = ~2MB, plus map overhead). Allow generous headroom
	// for GC timing variance.
	heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if heapGrowth < 0 {
		heapGrowth = 0
	}
	maxExpected := int64(200 << 20) // 200 MB
	if heapGrowth > maxExpected {
		t.Errorf("heap growth %d bytes exceeds %d bytes; possible memory leak",
			heapGrowth, maxExpected)
	}
	t.Logf("heap growth: %d bytes (%d KB)", heapGrowth, heapGrowth/1024)
}

// BenchmarkParallelEngine_FanOut measures FanOut throughput across different
// concurrency levels.
func BenchmarkParallelEngine_FanOut(b *testing.B) {
	concurrencies := []int{1, 4, 16, 64}
	const batchSize = 1000

	for _, c := range concurrencies {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			engine := NewParallelEngine(ParallelConfig{MaxConcurrent: c})
			items := make([]any, batchSize)
			for i := range items {
				items[i] = i
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
					n := item.(int)
					return n * 2, nil
				})
			}
		})
	}
}

// BenchmarkParallelEngine_FanOutVoid measures FanOutVoid throughput.
func BenchmarkParallelEngine_FanOutVoid(b *testing.B) {
	concurrencies := []int{1, 4, 16, 64}
	const batchSize = 1000

	for _, c := range concurrencies {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			engine := NewParallelEngine(ParallelConfig{MaxConcurrent: c})
			items := make([]any, batchSize)
			for i := range items {
				items[i] = i
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				engine.FanOutVoid(context.Background(), items, func(ctx context.Context, item any, idx int) error {
					return nil
				})
			}
		})
	}
}

// BenchmarkParallelEngine_FanOut_Latency measures per-item latency under
// different concurrencies with high-count batches.
func BenchmarkParallelEngine_FanOut_Latency(b *testing.B) {
	concurrencies := []int{1, 4, 16, 64}
	const batchSize = 10_000

	for _, c := range concurrencies {
		b.Run(fmt.Sprintf("concurrency=%d", c), func(b *testing.B) {
			engine := NewParallelEngine(ParallelConfig{MaxConcurrent: c})
			items := make([]any, batchSize)
			for i := range items {
				items[i] = i
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
					n := item.(int)
					return n * 2, nil
				})
			}
		})
	}
}
