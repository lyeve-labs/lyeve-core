package core

import (
	"errors"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// GoroutineTunables is the engine's scaling primitives (the goroutine
// tracker, the worker pool, the parallel engine and the async hook executor)
// as read views and live settings. A plugin that serves engine diagnostics
// reads them. The engine keeps the primitives, since every plugin shares
// them, and hands the plugin this role instead of the primitives themselves
// so the plugin never holds a pointer it could shut down or replace.
//
// Every read answers a view and whether the host has the primitive. A host
// built without one answers false and the zero view, so the plugin can
// serve a well-formed answer on an install built without that primitive
// rather than a 404.
//
// Every write applies to the running process without a restart and answers
// the view the matching read would return afterwards. Nothing persists: a
// restart boots from the configuration again. A field left at its zero
// value keeps the primitive's current setting. The primitive's own bounds
// decide what is accepted. An error that is not one of the sentinels below
// is the bounds message and safe to show, it carries no driver text.
type GoroutineTunables interface {
	// GoroutineStatus is the tracker snapshot: totals, goroutines by source
	// and by owner, and the leak limits.
	GoroutineStatus() (GoroutineSnapshot, bool)
	// PoolStats is the worker pool's size and counters.
	PoolStats() (GoroutinePoolView, bool)
	// ParallelConfig is the parallel engine's fan-out ceiling and deadline.
	ParallelConfig() (GoroutineParallelView, bool)
	// AsyncHookStatus is the async hook executor's switch, queue limits and
	// counters.
	AsyncHookStatus() (AsyncHookView, bool)

	// SetPoolSize resizes the worker pool. Growing admits queued work at
	// once. Shrinking lets running work finish.
	SetPoolSize(size int) (GoroutinePoolView, error)
	// SetParallelConfig reconfigures the parallel engine.
	SetParallelConfig(s GoroutineParallelSettings) (GoroutineParallelView, error)
	// SetAsyncHookConfig reconfigures the async hook executor's queue and,
	// when Enabled is set, switches the asynchronous path on or off.
	SetAsyncHookConfig(s AsyncHookSettings) (AsyncHookView, error)
}

// GoroutineTunablesProvider is implemented by the engine host and forwarded
// by ScopedHost. A plugin type-asserts it. A host built outside the engine
// may not implement it, and a plugin then serves the zero views.
type GoroutineTunablesProvider interface {
	GoroutineTunables() GoroutineTunables
}

// GoroutineSnapshot is the tracker's view, as GET /debug/goroutines answers
// it: total, runtime_total, by_source, by_owner, max_goroutines and
// leak_threshold.
type GoroutineSnapshot = engine.GoroutineSnapshot

// GoroutinePoolView is the worker pool as its stats route answers it.
type GoroutinePoolView struct {
	Size      int   `json:"size"`
	Active    int64 `json:"active"`
	Waiting   int   `json:"waiting"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	// Dropped is work the pool refused to start. A failure ran, a drop did
	// not, and a pool dropping most of what it is handed says the callers
	// are queuing under contexts that expire before a slot frees.
	Dropped     int64  `json:"dropped"`
	AvgLatency  string `json:"avg_latency"`
	TaskTimeout string `json:"task_timeout"`
}

// GoroutineParallelView is the parallel engine as its config route answers it.
type GoroutineParallelView struct {
	MaxConcurrent int    `json:"max_concurrent"`
	Timeout       string `json:"timeout"`
}

// GoroutineParallelSettings is a change to the parallel engine. A zero
// field keeps the current value.
type GoroutineParallelSettings struct {
	MaxConcurrent int
	Timeout       time.Duration
}

// AsyncHookView is the async hook executor as its status route answers it.
type AsyncHookView struct {
	Enabled   bool   `json:"enabled"`
	Workers   int    `json:"workers"`
	QueueSize int    `json:"queue_size"`
	Timeout   string `json:"timeout"`
	Queued    int64  `json:"queued"`
	Running   int64  `json:"running"`
	Overflow  int64  `json:"overflow"`
	Dropped   int64  `json:"dropped"`
}

// AsyncHookSettings is a change to the async hook executor. A zero field
// keeps the current value. A nil Enabled leaves the switch alone.
type AsyncHookSettings struct {
	Workers   int
	QueueSize int
	Timeout   time.Duration
	Enabled   *bool
}

// ErrScalingPrimitiveMissing is answered by a write on an install that runs
// without the primitive it would change. It is a conflict with how the
// install was built, not a bad request.
var ErrScalingPrimitiveMissing = errors.New("this install runs without that scaling primitive")

// ErrAsyncHooksNeedPool is answered by a switch of the asynchronous hook
// path on an install without a worker pool, which is where the hooks would
// run. It is the executor's own sentinel under the name a plugin reaches.
var ErrAsyncHooksNeedPool = engine.ErrNoWorkerPool

// NewGoroutineTunables adapts the scaling primitives a host exposes to the
// GoroutineTunables role. The engine host returns one of these. A fake host
// in a plugin's tests implements the role directly.
func NewGoroutineTunables(sh ScalingHost) GoroutineTunables {
	return scalingTunables{sh: sh}
}

type scalingTunables struct {
	sh ScalingHost
}

func (t scalingTunables) GoroutineStatus() (GoroutineSnapshot, bool) {
	if t.sh == nil || t.sh.GoroutineTracker() == nil {
		return GoroutineSnapshot{}, false
	}
	return t.sh.GoroutineTracker().Snapshot(), true
}

func (t scalingTunables) pool() *engine.WorkerPool {
	if t.sh == nil {
		return nil
	}
	return t.sh.WorkerPool()
}

func (t scalingTunables) parallel() *engine.ParallelEngine {
	if t.sh == nil {
		return nil
	}
	return t.sh.ParallelEngine()
}

func (t scalingTunables) asyncHooks() *engine.AsyncHookExecutor {
	if t.sh == nil {
		return nil
	}
	return t.sh.AsyncHookExecutor()
}

func poolView(wp *engine.WorkerPool) GoroutinePoolView {
	return GoroutinePoolView{
		Size:        wp.Size(),
		Active:      wp.Active(),
		Waiting:     wp.Waiting(),
		Completed:   wp.Completed(),
		Failed:      wp.Failed(),
		Dropped:     wp.Dropped(),
		AvgLatency:  wp.AvgLatency().String(),
		TaskTimeout: wp.TaskTimeout().String(),
	}
}

func parallelView(pe *engine.ParallelEngine) GoroutineParallelView {
	return GoroutineParallelView{
		MaxConcurrent: pe.MaxConcurrent(),
		Timeout:       pe.Timeout().String(),
	}
}

func asyncHookView(ex *engine.AsyncHookExecutor) AsyncHookView {
	cfg := ex.Config()
	stats := ex.Stats()
	return AsyncHookView{
		Enabled:   ex.Enabled(),
		Workers:   cfg.Workers,
		QueueSize: cfg.QueueSize,
		Timeout:   cfg.Timeout.String(),
		Queued:    stats.Queued,
		Running:   stats.Running,
		Overflow:  stats.Overflow,
		Dropped:   stats.Dropped,
	}
}

func (t scalingTunables) PoolStats() (GoroutinePoolView, bool) {
	wp := t.pool()
	if wp == nil {
		return GoroutinePoolView{}, false
	}
	return poolView(wp), true
}

func (t scalingTunables) ParallelConfig() (GoroutineParallelView, bool) {
	pe := t.parallel()
	if pe == nil {
		return GoroutineParallelView{}, false
	}
	return parallelView(pe), true
}

func (t scalingTunables) AsyncHookStatus() (AsyncHookView, bool) {
	ex := t.asyncHooks()
	if ex == nil {
		return AsyncHookView{}, false
	}
	return asyncHookView(ex), true
}

func (t scalingTunables) SetPoolSize(size int) (GoroutinePoolView, error) {
	wp := t.pool()
	if wp == nil {
		return GoroutinePoolView{}, ErrScalingPrimitiveMissing
	}
	if err := wp.Resize(size); err != nil {
		return GoroutinePoolView{}, err
	}
	return poolView(wp), nil
}

func (t scalingTunables) SetParallelConfig(s GoroutineParallelSettings) (GoroutineParallelView, error) {
	pe := t.parallel()
	if pe == nil {
		return GoroutineParallelView{}, ErrScalingPrimitiveMissing
	}
	cfg := pe.Config()
	if s.MaxConcurrent != 0 {
		cfg.MaxConcurrent = s.MaxConcurrent
	}
	if s.Timeout != 0 {
		cfg.Timeout = s.Timeout
	}
	if err := pe.Reconfigure(cfg); err != nil {
		return GoroutineParallelView{}, err
	}
	return parallelView(pe), nil
}

func (t scalingTunables) SetAsyncHookConfig(s AsyncHookSettings) (AsyncHookView, error) {
	ex := t.asyncHooks()
	if ex == nil {
		return AsyncHookView{}, ErrScalingPrimitiveMissing
	}
	cfg := ex.Config()
	if s.Workers != 0 {
		cfg.Workers = s.Workers
	}
	if s.QueueSize != 0 {
		cfg.QueueSize = s.QueueSize
	}
	if s.Timeout != 0 {
		cfg.Timeout = s.Timeout
	}
	if err := ex.Reconfigure(cfg); err != nil {
		return AsyncHookView{}, err
	}
	if s.Enabled != nil {
		if err := ex.SetEnabled(*s.Enabled); err != nil {
			return AsyncHookView{}, err
		}
	}
	return asyncHookView(ex), nil
}
