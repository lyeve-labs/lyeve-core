package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultShutdownTimeout bounds Shutdown. It matches the budget the engine
// already gives its goroutine tracker in the same shutdown sequence.
const DefaultShutdownTimeout = 5 * time.Second

// errTaskPanicked is what a waiter receives when the task it submitted panicked
// rather than returning. The panic itself is logged, with its stack, by the
// dispatcher that recovers it.
var errTaskPanicked = errors.New("worker pool task panicked")

// ErrPoolClosed is returned by SubmitAndWait when the pool has begun shutting
// down. The task was not started and will not be.
//
// The pool refuses late work rather than asking callers to stop submitting
// first. A caller that submits from a detached goroutine, as core.GoSafe does,
// cannot know whether a submit is still in flight when Shutdown runs.
var ErrPoolClosed = errors.New("worker pool is shutting down")

// MaxWorkerPoolSize bounds Resize. A pool wider than this is a request to
// run unbounded goroutines with extra steps, and the runtime's own
// scheduler is the better tool at that point.
const MaxWorkerPoolSize = 4096

// ErrInvalidPoolSize is returned by Resize for a size outside [1, MaxWorkerPoolSize].
var ErrInvalidPoolSize = errors.New("worker pool size must be between 1 and 4096")

// WorkerPoolConfig configures the centralized goroutine worker pool.
type WorkerPoolConfig struct {
	Size        int           // max concurrent goroutines, default 10
	TaskTimeout time.Duration // per-task deadline, zero means no timeout
}

// DefaultWorkerPoolConfig returns a safe default.
func DefaultWorkerPoolConfig() WorkerPoolConfig {
	return WorkerPoolConfig{Size: 100, TaskTimeout: 30 * time.Second}
}

// WorkerPool is a size-bounded goroutine pool. Submit returns immediately.
// The submitted function runs in a background goroutine that acquires a
// worker slot from sem. When all workers are busy, goroutines block on the
// acquire: this is the implicit queue, and Waiting counts it.
//
// One observable pool is shared by every caller. The size is set at boot and
// can be changed through Resize while the pool runs.
type WorkerPool struct {
	sem     *semaphore
	timeout time.Duration
	wg      sync.WaitGroup

	active    atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	dropped   atomic.Int64

	// warnDropped carries the first refusal to the log. Every refusal after it
	// is counted and not logged: a saturated pool refuses in the thousands, and
	// a line each would bury the one that matters.
	warnDropped sync.Once
	totalLat    atomic.Int64 // cumulative nanoseconds, all tasks

	// mu guards closed and serializes it against wg.Add. A WaitGroup may not
	// take an Add concurrently with a Wait once its counter has reached zero,
	// and a late submit racing Shutdown would do exactly that.
	mu     sync.Mutex
	closed bool
}

// NewWorkerPool creates a WorkerPool. When size <= 0, defaults to 10.
func NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	if cfg.Size <= 0 {
		cfg.Size = 10
	}
	return &WorkerPool{
		sem:     newSemaphore(cfg.Size),
		timeout: cfg.TaskTimeout,
	}
}

// Size returns the concurrency cap.
func (p *WorkerPool) Size() int { return p.sem.Limit() }

// Waiting returns how many submitted tasks are queued for a slot.
func (p *WorkerPool) Waiting() int { return p.sem.Waiting() }

// TaskTimeout returns the per-task deadline. Zero means none.
func (p *WorkerPool) TaskTimeout() time.Duration { return p.timeout }

// Resize changes the concurrency cap while the pool runs. Growing admits
// queued tasks at once. Shrinking lets the running tasks finish and admits
// nothing until the count is under the new cap. Nothing is dropped either
// way.
func (p *WorkerPool) Resize(size int) error {
	if size < 1 || size > MaxWorkerPoolSize {
		return ErrInvalidPoolSize
	}
	p.sem.Resize(size)
	return nil
}

// submit registers the task and starts it on its own goroutine, reporting
// whether the pool accepted it. The Add happens under mu so it cannot race a
// Shutdown's Wait.
//
// Registration belongs here rather than in dispatch. If dispatch called wg.Add
// only after winning a semaphore slot, a task still queued for a slot would be
// invisible to Shutdown: wg.Wait would return without it, the task would never
// run, and its goroutine would stay blocked on a semaphore nobody drains.
//
// The returned channel closes once dispatch has returned, which is after it has
// counted the task. A waiter that only reads the task's own result can outrun
// that: the result is sent from inside the task, while the failure is counted
// one frame above it, so SubmitAndWait would report a failure the pool had not
// recorded yet.
func (p *WorkerPool) submit(ctx context.Context, fn func(context.Context) error) (<-chan struct{}, bool) {
	settled := make(chan struct{})

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		close(settled)
		p.drop(ctx, "pool is shutting down")
		return settled, false
	}
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		defer close(settled)
		p.dispatch(ctx, fn)
	}()
	return settled, true
}

// Submit schedules fn for execution. Returns immediately, and fn runs in a
// background goroutine bounded by the concurrency cap.
func (p *WorkerPool) Submit(ctx context.Context, fn func(context.Context) error) {
	_, _ = p.submit(ctx, fn)
}

// SubmitAndWait enqueues fn and blocks until it completes or ctx is canceled.
func (p *WorkerPool) SubmitAndWait(ctx context.Context, fn func(context.Context) error) error {
	ch := make(chan error, 1)
	settled, accepted := p.submit(ctx, func(taskCtx context.Context) error {
		// The result has to reach the waiter even when fn panics. dispatch
		// recovers one frame above this, so an unguarded send would be skipped
		// entirely on that path and the select below could then only be
		// resolved by ctx expiring: a task that failed instantly would be
		// indistinguishable from one the pool never started, and the waiting
		// goroutine would stay parked until the deadline. The panic still travels
		// up to dispatch, which logs it with its own stack.
		delivered := false
		defer func() {
			if !delivered {
				ch <- errTaskPanicked
			}
		}()
		err := fn(taskCtx)
		ch <- err
		delivered = true
		return err
	})
	// A refused task never runs, so nothing will ever arrive on ch. Without
	// this the select below could only be resolved by ctx expiring, and the
	// caller would park until its deadline for work the pool had already
	// declined.
	if !accepted {
		return ErrPoolClosed
	}
	select {
	case err := <-ch:
		// The task has produced its result. Wait for the pool to finish
		// counting it so a caller that reads Failed or Completed straight
		// after this returns cannot see a total that excludes this task.
		<-settled
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubmitBatch fans out items to fn with bounded concurrency. Returns one
// error per item, in the same order. Uses SubmitAndWait internally so that
// context cancellation is properly reflected and the WaitGroup always resolves.
func (p *WorkerPool) SubmitBatch(ctx context.Context, items []any, fn func(context.Context, any, int) error) []error {
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		idx := i
		it := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[idx] = p.SubmitAndWait(ctx, func(taskCtx context.Context) error {
				return fn(taskCtx, it, idx)
			})
		}()
	}
	wg.Wait()
	return errs
}

// Active returns the number of currently executing tasks.
func (p *WorkerPool) Active() int64 { return p.active.Load() }

// Completed returns the cumulative count of successfully finished tasks.
func (p *WorkerPool) Completed() int64 { return p.completed.Load() }

// Failed returns the cumulative count of tasks that ran and errored or panicked.
// A task the pool refused to start is counted by Dropped, not here.
func (p *WorkerPool) Failed() int64 { return p.failed.Load() }

// Dropped returns the cumulative count of tasks the pool never ran because
// their context was done. They are kept apart from failures, which count work
// that was attempted, so a refusal never reads as ordinary failure noise.
func (p *WorkerPool) Dropped() int64 { return p.dropped.Load() }

// drop records a task the pool declined to start. Submit returns nothing, so
// without this the caller has no way to learn that its work never ran.
func (p *WorkerPool) drop(ctx context.Context, reason string) {
	p.dropped.Add(1)
	p.warnDropped.Do(func() {
		slog.WarnContext(ctx, "workerpool declined a task and will not run it",
			"reason", reason,
			"note", "further refusals are counted in the pool stats rather than logged")
	})
}

// AvgLatency returns the average task duration of completed tasks.
// Returns 0 when no tasks completed.
func (p *WorkerPool) AvgLatency() time.Duration {
	c := p.completed.Load()
	if c == 0 {
		return 0
	}
	return time.Duration(p.totalLat.Load() / c)
}

// Shutdown blocks until every submitted task has finished.
//
// Callers do not have to stop submitting first, and could not: a plugin that
// submits from a detached goroutine does not know whether one is still in
// flight. A submit arriving after this starts is refused and counted, and
// SubmitAndWait returns ErrPoolClosed rather than waiting out its deadline.
//
// It does not fill the semaphore to fence off new dispatches. A task queued
// behind such a fence could never acquire a slot, because nothing drains a
// semaphore that Shutdown filled.
func (p *WorkerPool) Shutdown() {
	if !p.ShutdownWithin(DefaultShutdownTimeout) {
		slog.Warn("workerpool shutdown timed out with tasks still running",
			"timeout", DefaultShutdownTimeout, "active", p.Active())
	}
}

// ShutdownWithin is Shutdown with a bound, reporting whether the pool drained.
//
// The wait runs last in the engine's shutdown, after both servers are closed
// and every plugin has stopped, so without a bound a single task that never
// returns would hold the process open with nothing left to report it.
func (p *WorkerPool) ShutdownWithin(timeout time.Duration) bool {
	// Close the door before waiting. Setting this under mu is what makes the
	// Wait below safe: no Add can be in progress once this returns.
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(drained)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
		return true
	case <-timer.C:
		return false
	}
}

// dispatch runs fn on the caller's goroutine once a worker slot is free. The
// caller is the goroutine submit started, and submit owns the WaitGroup, so
// dispatch must not register or release it.
//
// A task whose context is already done must not run at all. The select below
// cannot decide that on its own: when a worker slot is free and the context is
// already canceled, both cases are ready and Go picks one at random, so
// without the checks below abandoned work would run about half the time. The
// explicit checks around the acquire make cancellation authoritative and leave
// the wait to do what it is good at, which is blocking until whichever comes
// first when the pool is saturated.
func (p *WorkerPool) dispatch(ctx context.Context, fn func(context.Context) error) {
	if ctx.Err() != nil {
		p.drop(ctx, "context already done at submit")
		return
	}
	if err := p.sem.Acquire(ctx); err != nil {
		p.drop(ctx, "context done while waiting for a slot")
		return
	}
	defer p.sem.Release()

	// Waiting for a slot is unbounded, and the select can take the slot at the
	// same instant the context dies. Read it once more now that the work is
	// about to start.
	if ctx.Err() != nil {
		p.drop(ctx, "context done after taking a slot")
		return
	}

	defer func() {
		if r := recover(); r != nil {
			p.failed.Add(1)
			slog.Error("workerpool task panicked",
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)
		}
	}()

	p.active.Add(1)
	defer p.active.Add(-1)

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	start := time.Now()
	err := fn(ctx)
	p.totalLat.Add(int64(time.Since(start)))
	if err != nil {
		p.failed.Add(1)
	} else {
		p.completed.Add(1)
	}
}
