package enginehost

import (
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// WithWorkerPool wires a WorkerPool into the host. Call before plugin start.
func (h *engineHost) WithWorkerPool(p *engine.WorkerPool) { h.workerPool = p }

// WithGoroutineTracker wires a GoroutineTracker into the host.
func (h *engineHost) WithGoroutineTracker(t *engine.GoroutineTracker) { h.tracker = t }

// WithParallelEngine wires a ParallelEngine into the host.
func (h *engineHost) WithParallelEngine(e *engine.ParallelEngine) { h.parallel = e }

// WithAsyncHookExecutor wires an AsyncHookExecutor into the host.
func (h *engineHost) WithAsyncHookExecutor(e *engine.AsyncHookExecutor) { h.asyncHooks = e }
