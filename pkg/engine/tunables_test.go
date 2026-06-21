package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSemaphore_GrowAdmitsWaitersInOrder(t *testing.T) {
	s := newSemaphore(1)
	require.NoError(t, s.Acquire(context.Background()))

	woke := make([]chan struct{}, 3)
	for i := range woke {
		woke[i] = make(chan struct{})
		ready := woke[i]
		// Start each waiter after the previous one is queued so arrival
		// order is defined.
		waitFor(t, func() bool { return s.Waiting() == i }, "waiter queued")
		go func() {
			require.NoError(t, s.Acquire(context.Background()))
			close(ready)
		}()
	}
	waitFor(t, func() bool { return s.Waiting() == 3 }, "three waiters")

	// Grow one slot at a time: exactly the oldest waiter wakes each step.
	for i := range woke {
		s.Resize(2 + i)
		<-woke[i]
		for _, later := range woke[i+1:] {
			select {
			case <-later:
				t.Fatalf("waiter %d woke before its turn", i+1)
			default:
			}
		}
	}
	assert.Equal(t, 4, s.Held())
}

func TestSemaphore_ShrinkKeepsHoldersAndAdmitsNothingUntilUnder(t *testing.T) {
	s := newSemaphore(3)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.Acquire(context.Background()))
	}
	s.Resize(1)
	assert.Equal(t, 3, s.Held(), "a shrink evicts nobody")
	assert.False(t, s.TryAcquire())

	s.Release()
	s.Release()
	assert.False(t, s.TryAcquire(), "still one over the new limit")
	s.Release()
	assert.True(t, s.TryAcquire())
}

func TestSemaphore_CanceledWaiterLeavesNoSlotBehind(t *testing.T) {
	s := newSemaphore(1)
	require.NoError(t, s.Acquire(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Acquire(ctx) }()
	waitFor(t, func() bool { return s.Waiting() == 1 }, "waiter queued")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, 0, s.Waiting())

	s.Release()
	assert.Equal(t, 0, s.Held())
	assert.True(t, s.TryAcquire())
}

func TestSemaphore_ResizeUnderContention(t *testing.T) {
	s := newSemaphore(2)
	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, s.Acquire(context.Background()))
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			s.Release()
		}()
	}
	for _, n := range []int{8, 1, 16, 4} {
		time.Sleep(10 * time.Millisecond)
		s.Resize(n)
	}
	wg.Wait()
	assert.Equal(t, 0, s.Held())
	assert.Equal(t, 0, s.Waiting())
	assert.LessOrEqual(t, peak.Load(), int64(16))
}

func TestWorkerPool_ResizeAppliesToQueuedWork(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 1})
	release := make(chan struct{})
	var running atomic.Int64
	for i := 0; i < 4; i++ {
		pool.Submit(context.Background(), func(context.Context) error {
			running.Add(1)
			<-release
			return nil
		})
	}
	waitFor(t, func() bool { return pool.Active() == 1 && pool.Waiting() == 3 }, "one running, three queued")

	require.NoError(t, pool.Resize(4))
	waitFor(t, func() bool { return pool.Active() == 4 }, "the queue drained into the wider pool")
	assert.Equal(t, 4, pool.Size())

	require.NoError(t, pool.Resize(2))
	assert.Equal(t, int64(4), pool.Active(), "a shrink lets running work finish")
	close(release)
	require.True(t, pool.ShutdownWithin(2*time.Second))
	assert.Equal(t, int64(4), pool.Completed())
}

func TestWorkerPool_ResizeBounds(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 4})
	for _, size := range []int{0, -1, MaxWorkerPoolSize + 1} {
		assert.ErrorIs(t, pool.Resize(size), ErrInvalidPoolSize, "size %d", size)
	}
	assert.Equal(t, 4, pool.Size(), "a refused resize changes nothing")
	require.NoError(t, pool.Resize(MaxWorkerPoolSize))
	assert.Equal(t, MaxWorkerPoolSize, pool.Size())
}

func TestParallelEngine_ReconfigureAppliesToARunningFanOut(t *testing.T) {
	e := NewParallelEngine(ParallelConfig{MaxConcurrent: 1, Timeout: time.Second})
	release := make(chan struct{})
	var inFlight atomic.Int64
	items := []any{1, 2, 3, 4}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.FanOutVoid(context.Background(), items, func(context.Context, any, int) error {
			inFlight.Add(1)
			<-release
			return nil
		})
	}()
	waitFor(t, func() bool { return inFlight.Load() == 1 }, "one item running")
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(1), inFlight.Load(), "the ceiling of one holds")

	require.NoError(t, e.Reconfigure(ParallelConfig{MaxConcurrent: 4, Timeout: 2 * time.Second}))
	waitFor(t, func() bool { return inFlight.Load() == 4 }, "the fan-out widened live")
	assert.Equal(t, 4, e.MaxConcurrent())
	assert.Equal(t, 2*time.Second, e.Timeout())
	close(release)
	<-done
}

func TestParallelEngine_ReconfigureBounds(t *testing.T) {
	e := NewParallelEngine(ParallelConfig{MaxConcurrent: 2, Timeout: time.Second})
	for _, cfg := range []ParallelConfig{
		{MaxConcurrent: 0, Timeout: time.Second},
		{MaxConcurrent: MaxParallelConcurrency + 1, Timeout: time.Second},
		{MaxConcurrent: 2, Timeout: 0},
		{MaxConcurrent: 2, Timeout: MaxParallelTimeout + time.Second},
	} {
		assert.ErrorIs(t, e.Reconfigure(cfg), ErrInvalidParallelConfig, "%+v", cfg)
	}
	assert.Equal(t, ParallelConfig{MaxConcurrent: 2, Timeout: time.Second}, e.Config(), "a refused reconfigure changes nothing")
}

func TestAsyncHookExecutor_QueueOverflowRunsInline(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 8})
	ex := NewAsyncHookExecutor(pool, AsyncHookConfig{Workers: 1, QueueSize: 1, Timeout: time.Second})
	release := make(chan struct{})
	var async atomic.Int64
	block := func(context.Context) error { async.Add(1); <-release; return nil }

	// One runs, one waits in the queue. The third finds the queue full.
	ex.Run(context.Background(), "after_create", HookContext{}, block)
	waitFor(t, func() bool { return ex.Stats().Running == 1 }, "first hook running")
	ex.Run(context.Background(), "after_create", HookContext{}, block)
	waitFor(t, func() bool { return ex.Stats().Queued == 1 }, "second hook queued")

	inline := false
	ex.Run(context.Background(), "after_create", HookContext{}, func(context.Context) error { inline = true; return nil })
	assert.True(t, inline, "a hook past the queue runs in the caller")
	assert.Equal(t, int64(1), ex.Stats().Overflow)

	close(release)
	waitFor(t, func() bool { return ex.Stats().Running == 0 && ex.Stats().Queued == 0 }, "queue drained")
	assert.Equal(t, int64(2), async.Load())
}

func TestAsyncHookExecutor_ReconfigureWidensLive(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 8})
	ex := NewAsyncHookExecutor(pool, AsyncHookConfig{Workers: 1, QueueSize: 8, Timeout: time.Second})
	release := make(chan struct{})
	for i := 0; i < 3; i++ {
		ex.Run(context.Background(), "after_update", HookContext{}, func(context.Context) error { <-release; return nil })
	}
	waitFor(t, func() bool { s := ex.Stats(); return s.Running == 1 && s.Queued == 2 }, "one running, two queued")

	require.NoError(t, ex.Reconfigure(AsyncHookConfig{Workers: 3, QueueSize: 8, Timeout: time.Second}))
	waitFor(t, func() bool { return ex.Stats().Running == 3 }, "the extra workers took the queue")
	assert.Equal(t, AsyncHookConfig{Workers: 3, QueueSize: 8, Timeout: time.Second}, ex.Config())
	close(release)
	waitFor(t, func() bool { return ex.Stats().Running == 0 }, "hooks finished")
}

func TestAsyncHookExecutor_ReconfigureBoundsAndEnable(t *testing.T) {
	ex := MakeAsyncHookExecutor(nil)
	assert.False(t, ex.Enabled())
	assert.ErrorIs(t, ex.SetEnabled(true), ErrNoWorkerPool)
	for _, cfg := range []AsyncHookConfig{
		{Workers: 0, QueueSize: 1, Timeout: time.Second},
		{Workers: MaxAsyncHookWorkers + 1, QueueSize: 1, Timeout: time.Second},
		{Workers: 1, QueueSize: 0, Timeout: time.Second},
		{Workers: 1, QueueSize: MaxAsyncHookQueueSize + 1, Timeout: time.Second},
		{Workers: 1, QueueSize: 1, Timeout: 0},
		{Workers: 1, QueueSize: 1, Timeout: MaxAsyncHookTimeout + time.Second},
	} {
		assert.ErrorIs(t, ex.Reconfigure(cfg), ErrInvalidAsyncHookConfig, "%+v", cfg)
	}
	assert.Equal(t, DefaultAsyncHookConfig(), ex.Config(), "a refused reconfigure changes nothing")

	withPool := MakeAsyncHookExecutor(NewWorkerPool(WorkerPoolConfig{Size: 2}))
	require.NoError(t, withPool.SetEnabled(false))
	assert.False(t, withPool.Enabled())
	require.NoError(t, withPool.SetEnabled(true))
	assert.True(t, withPool.Enabled())
}

func TestAsyncHookExecutor_ClosedPoolReturnsTheQueueSlot(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2})
	ex := NewAsyncHookExecutor(pool, AsyncHookConfig{Workers: 1, QueueSize: 1, Timeout: time.Second})
	require.True(t, pool.ShutdownWithin(time.Second))

	ex.Run(context.Background(), "after_delete", HookContext{}, func(context.Context) error { return errors.New("never runs") })
	assert.Equal(t, int64(1), ex.Stats().Dropped)
	assert.Equal(t, int64(0), ex.Stats().Queued, "the refused hook gave its slot back")
}

func TestGoroutineTracker_SnapshotCountsByOwner(t *testing.T) {
	root := NewGoroutineTracker(DefaultTrackerConfig())
	defer func() { _ = root.Shutdown(time.Second) }()
	ai := root.ForOwner("ai")
	cron := root.ForOwner("cron")
	assert.Equal(t, OwnerEngine, root.Owner())
	assert.Equal(t, "ai", ai.Owner())
	assert.Same(t, root, root.ForOwner(""), "an empty owner is the receiver")

	block := func(ctx context.Context) { <-ctx.Done() }
	root.Go("sweeper", block)
	ai.Go("transcript-sweeper", block)
	ai.Go("embedding-queue", block)
	cron.Go("scheduler", block)
	waitFor(t, func() bool { return root.Count() == 4 }, "four tracked")

	snap := cron.Snapshot()
	assert.Equal(t, 4, snap.Total, "every view sees the shared entries")
	assert.Equal(t, 1, snap.ByOwner[OwnerEngine].Total)
	assert.Equal(t, 2, snap.ByOwner["ai"].Total)
	assert.Equal(t, 1, snap.ByOwner["cron"].Total)
	assert.NotEmpty(t, snap.ByOwner["ai"].Oldest)
	assert.Equal(t, 2, snap.BySource["transcript-sweeper"]+snap.BySource["embedding-queue"])
}
