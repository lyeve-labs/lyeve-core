package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWorkerPool_Stress_10000Tasks submits 10000 tasks through a pool of 50
// workers and verifies all complete without deadlocks.
func TestWorkerPool_Stress_10000Tasks(t *testing.T) {
	t.Parallel()

	const (
		numTasks = 10000
		poolSize = 50
	)

	pool := NewWorkerPool(WorkerPoolConfig{Size: poolSize, TaskTimeout: 30 * time.Second})
	var completed atomic.Int64
	ctx := context.Background()

	for i := 0; i < numTasks; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			completed.Add(1)
			return nil
		})
	}

	// Shutdown rather than a WaitGroup the tasks release themselves. The pool
	// counts a task after its function returns, so a WaitGroup signaled from
	// inside that function is already done when the counter is still one
	// short. Shutdown waits on the pool's own accounting, which is what the
	// assertions below read.
	pool.Shutdown()

	if n := completed.Load(); n != numTasks {
		t.Errorf("expected %d tasks completed, got %d", numTasks, n)
	}
	if f := pool.Failed(); f != 0 {
		t.Errorf("expected 0 failed tasks, got %d", f)
	}
	if c := pool.Completed(); c != numTasks {
		t.Errorf("expected pool.Completed() == %d, got %d", numTasks, c)
	}
}

// TestWorkerPool_ConcurrentSubmitters launches 100 goroutines each submitting
// 100 tasks concurrently, then verifies the total count.
func TestWorkerPool_ConcurrentSubmitters(t *testing.T) {
	t.Parallel()

	const (
		numGoroutines = 100
		tasksPerGr    = 100
		totalTasks    = numGoroutines * tasksPerGr
		poolSize      = 50
	)

	pool := NewWorkerPool(WorkerPoolConfig{Size: poolSize, TaskTimeout: 30 * time.Second})
	var completed atomic.Int64
	ctx := context.Background()

	var submitters sync.WaitGroup
	submitters.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer submitters.Done()
			for i := 0; i < tasksPerGr; i++ {
				pool.Submit(ctx, func(ctx context.Context) error {
					completed.Add(1)
					return nil
				})
			}
		}()
	}

	// Every task has to be submitted before Shutdown is allowed to decide the
	// pool is idle, so the submitters are waited on first.
	submitters.Wait()
	pool.Shutdown()

	if n := completed.Load(); n != totalTasks {
		t.Errorf("expected %d tasks completed, got %d", totalTasks, n)
	}
	if f := pool.Failed(); f != 0 {
		t.Errorf("expected 0 failed tasks, got %d", f)
	}
}

// TestWorkerPool_ContextCancellation submits tasks with an already-canceled
// context and verifies they are rejected (failed counter increments).
func TestWorkerPool_ContextCancellation(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})

	// Fill the sem to capacity so canceled-context tasks have no chance of
	// acquiring a slot (select would non-deterministically pick sem<- over
	// <-ctx.Done() if both are ready).
	blockCh := make(chan struct{})
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			<-blockCh
			return nil
		})
	}
	// Give the blocking tasks time to acquire all sem slots.
	time.Sleep(50 * time.Millisecond)

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup
	const rejectedTasks = 20
	wg.Add(rejectedTasks)
	for i := 0; i < rejectedTasks; i++ {
		pool.Submit(cancelCtx, func(ctx context.Context) error {
			defer wg.Done()
			return nil
		})
	}

	// Tasks submitted with canceled context should never run. Their wg.Done
	// would never be called. Use a timeout to verify they don't complete.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Error("tasks with canceled context should not have run")
	case <-time.After(500 * time.Millisecond):
		// Expected: wg.Wait never returns because tasks were rejected.
	}

	// A task the pool refused to start never ran, so it is counted as dropped
	// rather than failed. Counting it as a failure would read as work that was
	// attempted and would hide the refusals among genuine errors.
	if d := pool.Dropped(); d < int64(rejectedTasks) {
		t.Errorf("expected at least %d dropped tasks, got %d", rejectedTasks, d)
	}
	if f := pool.Failed(); f != 0 {
		t.Errorf("no task ran, so none can have failed, got %d", f)
	}

	// Release blocked tasks so the pool can drain.
	close(blockCh)
	pool.Shutdown()
}

// TestWorkerPool_ShutdownDrainsAll submits tasks, calls Shutdown, and verifies
// all tasks complete before Shutdown returns.
func TestWorkerPool_ShutdownDrainsAll(t *testing.T) {
	t.Parallel()

	const (
		numTasks = 100
		poolSize = 20
	)

	pool := NewWorkerPool(WorkerPoolConfig{Size: poolSize, TaskTimeout: 30 * time.Second})
	var completed atomic.Int64
	ctx := context.Background()

	// Submit tasks that are fast enough to be practical but slow enough that
	// some are still running when Shutdown is called.
	for i := 0; i < numTasks; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			time.Sleep(time.Millisecond)
			completed.Add(1)
			return nil
		})
	}

	// Give a few tasks time to start.
	time.Sleep(10 * time.Millisecond)

	pool.Shutdown()

	if n := completed.Load(); n != numTasks {
		t.Errorf("expected %d tasks completed after shutdown, got %d", numTasks, n)
	}
}

// TestWorkerPool_PanicRecovery submits a task that panics and verifies the pool
// stays alive and can still process tasks.
func TestWorkerPool_PanicRecovery(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})
	ctx := context.Background()

	// Submit a task that panics.
	pool.Submit(ctx, func(ctx context.Context) error {
		panic("intentional panic for test")
	})

	// Give the panic time to be caught and logged.
	time.Sleep(50 * time.Millisecond)

	if f := pool.Failed(); f < 1 {
		t.Errorf("expected at least 1 failed task after panic, got %d", f)
	}

	// Verify the pool still works.
	var completed atomic.Int64
	const followUpTasks = 50
	var wg sync.WaitGroup
	wg.Add(followUpTasks)
	for i := 0; i < followUpTasks; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			defer wg.Done()
			completed.Add(1)
			return nil
		})
	}

	wg.Wait()
	if n := completed.Load(); n != followUpTasks {
		t.Errorf("expected %d follow-up tasks completed, got %d", followUpTasks, n)
	}
}

// TestWorkerPool_SubmitAndWait_Timeout verifies that SubmitAndWait returns
// context.DeadlineExceeded when the caller's context expires before the task
// completes.
func TestWorkerPool_SubmitAndWait_Timeout(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := pool.SubmitAndWait(ctx, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	})

	if err == nil {
		t.Error("expected timeout error from SubmitAndWait, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestWorkerPool_ActiveCount verifies the Active() metric reflects in-flight
// task count accurately.
func TestWorkerPool_ActiveCount(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})

	blockCh := make(chan struct{})
	ctx := context.Background()

	// Submit 4 blocking tasks to fill all workers.
	for i := 0; i < 4; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			<-blockCh
			return nil
		})
	}

	// Wait for tasks to become active.
	time.Sleep(50 * time.Millisecond)

	// Active count is approximate (incremented inside the worker goroutine)
	// but should be at least 1 after 50ms.
	if a := pool.Active(); a < 1 {
		t.Errorf("expected Active() >= 1, got %d", a)
	}

	close(blockCh)
	pool.Shutdown()

	if a := pool.Active(); a != 0 {
		t.Errorf("expected Active() == 0 after shutdown, got %d", a)
	}
}

// TestWorkerPool_AvgLatency verifies the AvgLatency metric is populated after
// tasks complete.
func TestWorkerPool_AvgLatency(t *testing.T) {
	t.Parallel()

	pool := NewWorkerPool(WorkerPoolConfig{Size: 4, TaskTimeout: 5 * time.Second})
	ctx := context.Background()

	if lat := pool.AvgLatency(); lat != 0 {
		t.Errorf("expected zero AvgLatency with no tasks, got %v", lat)
	}

	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			return nil
		})
	}
	wg.Wait()

	if lat := pool.AvgLatency(); lat == 0 {
		t.Error("expected non-zero AvgLatency after tasks")
	}
}

// BenchmarkWorkerPool_Submit measures Submit throughput under sustained load.
func BenchmarkWorkerPool_Submit(b *testing.B) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 100, TaskTimeout: 30 * time.Second})
	ctx := context.Background()

	var completed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(b.N)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Submit(ctx, func(ctx context.Context) error {
			completed.Add(1)
			wg.Done()
			return nil
		})
	}
	wg.Wait()
	b.StopTimer()

	_ = completed.Load()
}

// BenchmarkWorkerPool_SubmitAndWait measures per-task SubmitAndWait latency.
func BenchmarkWorkerPool_SubmitAndWait(b *testing.B) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 100, TaskTimeout: 30 * time.Second})
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pool.SubmitAndWait(ctx, func(ctx context.Context) error {
			return nil
		})
	}
	b.StopTimer()
}

// BenchmarkWorkerPool_SubmitBatch measures SubmitBatch throughput for bulk
// item processing.
func BenchmarkWorkerPool_SubmitBatch(b *testing.B) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 100, TaskTimeout: 30 * time.Second})
	ctx := context.Background()

	items := make([]any, b.N)
	for i := range items {
		items[i] = i
	}

	b.ResetTimer()
	_ = pool.SubmitBatch(ctx, items, func(ctx context.Context, item any, idx int) error {
		return nil
	})
	b.StopTimer()
}
