package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// AsyncHookConfig configures asynchronous hook execution.
type AsyncHookConfig struct {
	// Timeout is the per-hook deadline. Default 5s.
	Timeout time.Duration
	// Workers is how many hooks run at once. Default 8. The pool bounds it
	// again from above: a hook occupies a pool slot while it runs, so a
	// Workers count wider than the pool is the pool's width in practice.
	Workers int
	// QueueSize is how many hooks may wait for a worker. Default 1024. A
	// hook arriving at a full queue runs synchronously in the caller, the
	// way it would on an install with async hooks off, so back-pressure
	// costs the writer latency and never costs the hook.
	QueueSize int
}

// Bounds on Reconfigure. Hooks are per write, so a thousand concurrent ones
// means the writes themselves are the problem. The queue is sized so a
// burst of writes can outrun a slow hook target without the writer feeling
// it, and a bigger one only hides that the target is down.
const (
	MaxAsyncHookWorkers   = 1024
	MaxAsyncHookQueueSize = 65536
	MaxAsyncHookTimeout   = 10 * time.Minute
)

// ErrInvalidAsyncHookConfig is returned by Reconfigure for a config outside
// its bounds.
var ErrInvalidAsyncHookConfig = errors.New("async hook config out of bounds: workers in [1, 1024], queue_size in [1, 65536], timeout in (0, 10m]")

// ErrNoWorkerPool is returned when an executor built without a pool is
// asked to run hooks asynchronously. There is nothing to run them on.
var ErrNoWorkerPool = errors.New("async hooks need a worker pool")

// DefaultAsyncHookConfig returns a safe default.
func DefaultAsyncHookConfig() AsyncHookConfig {
	return AsyncHookConfig{Timeout: 5 * time.Second, Workers: 8, QueueSize: 1024}
}

func (c AsyncHookConfig) withDefaults() AsyncHookConfig {
	d := DefaultAsyncHookConfig()
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.Workers <= 0 {
		c.Workers = d.Workers
	}
	if c.QueueSize <= 0 {
		c.QueueSize = d.QueueSize
	}
	return c
}

func (c AsyncHookConfig) valid() bool {
	return c.Workers >= 1 && c.Workers <= MaxAsyncHookWorkers &&
		c.QueueSize >= 1 && c.QueueSize <= MaxAsyncHookQueueSize &&
		c.Timeout > 0 && c.Timeout <= MaxAsyncHookTimeout
}

// AsyncHookExecutor runs after-action hooks (AfterCreate, AfterUpdate,
// AfterDelete, AfterResponse) asynchronously via WorkerPool.
//
// Before hooks remain synchronous: they can veto the request with 403/422.
// After hooks are best-effort. When the executor is disabled they run in the
// request goroutine, adding their latency to every write.
//
// Two limits sit in front of the pool. workers bounds how many hooks run at
// once, so a burst of writes cannot take every pool slot from the plugins
// that share it. Queue bounds how many hooks wait for a worker, and a hook
// that finds the queue full runs inline instead. Both are set at boot and
// change live through Reconfigure.
type AsyncHookExecutor struct {
	pool    *WorkerPool
	timeout atomic.Int64 // nanoseconds
	enabled atomic.Bool
	workers *semaphore
	queue   *semaphore

	queued   atomic.Int64 // admitted to the queue, not yet running
	running  atomic.Int64 // inside a hook
	overflow atomic.Int64 // queue was full, so the hook ran inline
	dropped  atomic.Int64 // no worker within the timeout, so the hook did not run
}

// NewAsyncHookExecutor creates an executor backed by pool. When pool is nil,
// the executor is a no-op (sync path). Zero fields in cfg take the defaults.
func NewAsyncHookExecutor(pool *WorkerPool, cfg AsyncHookConfig) *AsyncHookExecutor {
	cfg = cfg.withDefaults()
	e := &AsyncHookExecutor{
		pool:    pool,
		workers: newSemaphore(cfg.Workers),
		queue:   newSemaphore(cfg.QueueSize),
	}
	e.timeout.Store(int64(cfg.Timeout))
	e.enabled.Store(pool != nil)
	return e
}

// HookContext carries the minimal request identity needed for async hook
// execution. The parent request context is not propagated (hooks use
// context.WithoutCancel) so that client disconnection does not abort hooks.
type HookContext struct {
	TenantID  string
	UserID    string
	RequestID string
	TraceID   string
}

// Run executes the hook. When the executor is enabled, the hook runs
// asynchronously via WorkerPool. When disabled, or when the queue is full,
// the hook runs synchronously.
//
// name is the hook type (e.g. "after_create") for logging/metrics.
// fn is the hook function to execute.
func (e *AsyncHookExecutor) Run(ctx context.Context, name string, hc HookContext, fn func(context.Context) error) {
	if !e.enabled.Load() {
		e.runInline(ctx, name, hc, fn)
		return
	}
	if !e.queue.TryAcquire() {
		e.overflow.Add(1)
		e.runInline(ctx, name, hc, fn)
		return
	}
	e.queued.Add(1)

	// Detach before submitting, not inside the task. The pool refuses to start
	// a task whose context is done, and the context it reads is the one it was
	// handed here, not the one the closure builds later. Detaching inside the
	// closure would protect nothing on the path that matters: a caller that
	// went away while its hook was still queued would have that hook dropped and
	// counted, which is the opposite of what this executor promises one line
	// above. Values carry across WithoutCancel, so the tenant, user and trace
	// identity are unchanged.
	detached := context.WithoutCancel(ctx)

	// The timeout is created inside the closure so it lives for the lifetime
	// of the async task, not the lifetime of this Run call. The inner detach
	// keeps the pool's per-task deadline on taskCtx off the hook. The wait for
	// a worker shares the hook's
	// deadline, so a hook that cannot start in time is dropped and counted
	// rather than parked forever.
	_, accepted := e.pool.submit(detached, func(taskCtx context.Context) error {
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(taskCtx), e.Timeout())
		defer cancel()

		if err := e.workers.Acquire(hookCtx); err != nil {
			e.queue.Release()
			e.queued.Add(-1)
			e.dropped.Add(1)
			slog.Warn("async hook dropped: no worker within the timeout",
				"hook", name, "tenant", hc.TenantID, "timeout", e.Timeout().String())
			return err
		}
		e.queue.Release()
		e.queued.Add(-1)
		e.running.Add(1)
		defer func() {
			e.running.Add(-1)
			e.workers.Release()
		}()

		if err := fn(hookCtx); err != nil {
			slog.Warn("async hook failed",
				"hook", name,
				"tenant", hc.TenantID,
				"err", err)
			return err
		}
		return nil
	})
	// A pool that is shutting down refuses the task, and a refused task
	// never runs, so its queue slot has to come back here.
	if !accepted {
		e.queue.Release()
		e.queued.Add(-1)
		e.dropped.Add(1)
	}
}

func (e *AsyncHookExecutor) runInline(ctx context.Context, name string, hc HookContext, fn func(context.Context) error) {
	if err := fn(ctx); err != nil {
		slog.Warn("sync hook failed", "hook", name, "tenant", hc.TenantID, "err", err)
	}
}

// Enabled reports whether the executor dispatches hooks asynchronously.
func (e *AsyncHookExecutor) Enabled() bool { return e.enabled.Load() }

// SetEnabled switches between the asynchronous and the inline path. It
// refuses to enable an executor that has no pool to run on.
func (e *AsyncHookExecutor) SetEnabled(on bool) error {
	if on && e.pool == nil {
		return ErrNoWorkerPool
	}
	e.enabled.Store(on)
	return nil
}

// Timeout returns the per-hook deadline.
func (e *AsyncHookExecutor) Timeout() time.Duration { return time.Duration(e.timeout.Load()) }

// Config returns the live configuration.
func (e *AsyncHookExecutor) Config() AsyncHookConfig {
	return AsyncHookConfig{
		Timeout:   e.Timeout(),
		Workers:   e.workers.Limit(),
		QueueSize: e.queue.Limit(),
	}
}

// Reconfigure applies cfg to a running executor. Hooks already queued keep
// their place. A smaller queue admits nothing new until the backlog is
// under it, and a smaller worker count lets the running hooks finish.
func (e *AsyncHookExecutor) Reconfigure(cfg AsyncHookConfig) error {
	if !cfg.valid() {
		return ErrInvalidAsyncHookConfig
	}
	e.workers.Resize(cfg.Workers)
	e.queue.Resize(cfg.QueueSize)
	e.timeout.Store(int64(cfg.Timeout))
	return nil
}

// AsyncHookStats is a point-in-time view of the executor's queue.
type AsyncHookStats struct {
	Queued   int64
	Running  int64
	Overflow int64
	Dropped  int64
}

// Stats returns the live counters.
func (e *AsyncHookExecutor) Stats() AsyncHookStats {
	return AsyncHookStats{
		Queued:   e.queued.Load(),
		Running:  e.running.Load(),
		Overflow: e.overflow.Load(),
		Dropped:  e.dropped.Load(),
	}
}

// MakeAsyncHookExecutor is a convenience constructor that wires up a
// WorkerPool with the default async hook configuration. Returns a disabled
// executor when pool is nil.
func MakeAsyncHookExecutor(pool *WorkerPool) *AsyncHookExecutor {
	return NewAsyncHookExecutor(pool, DefaultAsyncHookConfig())
}
