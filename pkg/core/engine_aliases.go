package core

import "github.com/lyeve-labs/lyeve-core/pkg/engine"

// Engine type aliases let a plugin reach the concurrency types of pkg/engine
// through core.
type (
	GoroutineTracker  = engine.GoroutineTracker
	WorkerPool        = engine.WorkerPool
	WorkerPoolConfig  = engine.WorkerPoolConfig
	ParallelEngine    = engine.ParallelEngine
	AsyncHookExecutor = engine.AsyncHookExecutor
	DistLock          = engine.DistLock
	DistLockConfig    = engine.DistLockConfig
)

// NewWorkerPool constructs an engine worker pool.
var NewWorkerPool = engine.NewWorkerPool

// DefaultWorkerPoolConfig returns the default worker pool configuration.
var DefaultWorkerPoolConfig = engine.DefaultWorkerPoolConfig

// Function aliases for engine symbols frequently used by plugins.
var (
	GoSafe = engine.GoSafe
)
