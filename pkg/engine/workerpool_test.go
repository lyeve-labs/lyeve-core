package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerPool_Submit(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})

	var counter atomic.Int32
	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		pool.Submit(ctx, func(ctx context.Context) error {
			defer wg.Done()
			counter.Add(1)
			return nil
		})
	}

	// Wait for all tasks to complete.
	wg.Wait()

	if n := counter.Load(); n != 20 {
		t.Errorf("expected 20 tasks, got %d", n)
	}
}

func TestWorkerPool_SubmitAndWait(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})

	err := pool.SubmitAndWait(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	taskErr := errors.New("task failed")
	err = pool.SubmitAndWait(context.Background(), func(ctx context.Context) error {
		return taskErr
	})
	if !errors.Is(err, taskErr) {
		t.Errorf("expected taskErr, got %v", err)
	}
}

func TestWorkerPool_SubmitBatch(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 8, TaskTimeout: 5 * time.Second})

	items := make([]any, 100)
	for i := range items {
		items[i] = i
	}

	var mu sync.Mutex
	seen := make(map[int]bool)
	errs := pool.SubmitBatch(context.Background(), items, func(ctx context.Context, item any, idx int) error {
		mu.Lock()
		seen[item.(int)] = true
		mu.Unlock()
		return nil
	})

	for _, e := range errs {
		if e != nil {
			t.Errorf("unexpected error: %v", e)
		}
	}
	if len(seen) != 100 {
		t.Errorf("expected 100 items processed, got %d", len(seen))
	}
}

func TestWorkerPool_Timeout(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2, TaskTimeout: 10 * time.Millisecond})

	err := pool.SubmitAndWait(context.Background(), func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	})
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestWorkerPool_Metrics(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})

	for i := 0; i < 10; i++ {
		pool.SubmitAndWait(context.Background(), func(ctx context.Context) error {
			return nil
		})
	}

	if pool.Completed() != 10 {
		t.Errorf("expected 10 completed, got %d", pool.Completed())
	}
	if pool.Failed() != 0 {
		t.Errorf("expected 0 failed, got %d", pool.Failed())
	}
}

func TestWorkerPool_ZeroValues(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{})
	if pool == nil {
		t.Fatal("expected non-nil pool")
	}
	// Default size should be 10.
	err := pool.SubmitAndWait(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Submitting while the pool shuts down

// A detached submitter racing Shutdown is the shape that would race wg.Add
// against wg.Wait. Run with -race, which is how every suite here runs.
func TestWorkerPool_SubmitRacingShutdown(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: time.Second})

	// Submitters that arrive on their own schedule, as core.GoSafe callers do.
	const submitters = 32
	var started, finished sync.WaitGroup
	started.Add(submitters)
	finished.Add(submitters)

	var mu sync.Mutex
	outcomes := make([]error, 0, submitters)

	for i := 0; i < submitters; i++ {
		go func() {
			defer finished.Done()
			started.Done()
			err := pool.SubmitAndWait(context.Background(), func(context.Context) error {
				return nil
			})
			mu.Lock()
			outcomes = append(outcomes, err)
			mu.Unlock()
		}()
	}
	started.Wait()
	pool.Shutdown()
	finished.Wait()

	// Racing a shutdown has exactly two acceptable answers: the task ran, or
	// the pool refused it. Which of the two a submitter gets is timing, and
	// most runs see no refusal at all, so the check below is a bound on the
	// outcomes rather than coverage of the refusal path.
	// TestWorkerPool_SubmitAfterShutdown_ReturnsImmediately covers that
	// deliberately.
	//
	// What this test is for is the race detector, which reports wg.Add racing
	// wg.Wait. The count assertion is the liveness half: a submitter
	// that never settles hangs here rather than being counted as a pass.
	if len(outcomes) != submitters {
		t.Fatalf("collected %d outcomes, want %d", len(outcomes), submitters)
	}
	for _, err := range outcomes {
		if err != nil && !errors.Is(err, ErrPoolClosed) {
			t.Fatalf("submitter racing shutdown returned %v, want nil or ErrPoolClosed", err)
		}
	}
}

// A refused task must not leave its caller waiting out the deadline. If nothing
// arrived on the result channel for work the pool never started, the only thing
// that could resolve the wait would be ctx expiring.
func TestWorkerPool_SubmitAfterShutdown_ReturnsImmediately(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2, TaskTimeout: time.Minute})
	pool.Shutdown()

	ran := false
	start := time.Now()
	err := pool.SubmitAndWait(context.Background(), func(context.Context) error {
		ran = true
		return nil
	})

	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("expected ErrPoolClosed, got %v", err)
	}
	if ran {
		t.Error("a refused task ran anyway")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("refusal took %s; it should not wait on anything", elapsed)
	}
	if d := pool.Dropped(); d != 1 {
		t.Errorf("expected the refusal to be counted once, got %d", d)
	}
}

// Fire-and-forget Submit after shutdown must be a no-op rather than a panic on
// a WaitGroup reused after Wait.
func TestWorkerPool_FireAndForgetAfterShutdown(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2})
	pool.Shutdown()

	ran := atomic.Bool{}
	pool.Submit(context.Background(), func(context.Context) error {
		ran.Store(true)
		return nil
	})
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Error("a task submitted after shutdown ran")
	}
}
