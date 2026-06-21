package engine

import (
	"context"
	"testing"
	"time"
)

// A waiter has to hear back even when the task panics. dispatch recovers the
// panic a frame above the closure SubmitAndWait submits. An unguarded send
// would be skipped on that path, the caller's select could then only end on
// its own context, and a task that failed instantly would look like one the
// pool never started.
func TestSubmitAndWait_ReportsATaskThatPanicked(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2})
	defer pool.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := pool.SubmitAndWait(ctx, func(context.Context) error {
		panic("task exploded")
	})

	if err == nil {
		t.Fatal("a task that panicked must not report success")
	}
	if ctx.Err() != nil {
		t.Fatal("the wait must end on the task, not on the context expiring")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the report must arrive with the panic, took %s", elapsed)
	}
	if got := pool.Failed(); got != 1 {
		t.Errorf("a panic is a task that ran and failed, want failed=1 got %d", got)
	}
	if got := pool.Dropped(); got != 0 {
		t.Errorf("the task ran, so nothing was dropped, got %d", got)
	}
}

// Shutdown runs last in the engine's shutdown, after both servers are closed
// and every plugin has stopped, so an unbounded wait would let one task that
// never returns hold the process open with nothing left running to say why.
func TestShutdownWithin_ReturnsWhenATaskWillNotFinish(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 1})

	release := make(chan struct{})
	running := make(chan struct{})
	pool.Submit(context.Background(), func(context.Context) error {
		close(running)
		<-release
		return nil
	})
	<-running

	if pool.ShutdownWithin(200 * time.Millisecond) {
		t.Fatal("the pool has a task still running, so it cannot report drained")
	}

	close(release)
	if !pool.ShutdownWithin(5 * time.Second) {
		t.Error("the task finished, so the pool must drain")
	}
}
