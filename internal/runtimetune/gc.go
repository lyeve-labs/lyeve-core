// Package runtimetune holds the process's runtime and scaling settings: the
// scheduler's thread count, Go's soft memory limit, the garbage collector's
// target and the primary database pool's size. It serves them to plugins as
// core.RuntimeTuning, follows the cgroup's limits while the process runs, and
// keeps the rule the engine's own garbage collector endpoint shares.
package runtimetune

import (
	"errors"
	"math"
	"runtime/debug"
	"runtime/metrics"
)

// ErrCollectorOffNeedsLimit refuses to turn the garbage collector off, or to
// remove the memory limit while it is off. Without a limit nothing bounds the
// heap short of the kernel's OOM kill.
var ErrCollectorOffNeedsLimit = errors.New("gogc below zero turns the garbage collector off, which needs a memory limit")

// GCSettings returns the collector percentage and the memory limit in force,
// with 0 for no memory limit. The percentage is read from runtime/metrics,
// because debug.SetGCPercent reports it only by replacing it.
func GCSettings() (gogc, memLimit int64) {
	sample := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(sample)
	memLimit = debug.SetMemoryLimit(-1)
	if memLimit == math.MaxInt64 {
		memLimit = 0
	}
	return int64(sample[0].Value.Uint64()), memLimit
}

// SetGCPercent sets the collector percentage and returns the one it
// replaced. A negative percentage is refused with ErrCollectorOffNeedsLimit
// unless a memory limit is in force, and the percentage in force stays.
func SetGCPercent(percent int) (int, error) {
	if _, memLimit := GCSettings(); percent < 0 && memLimit == 0 {
		return 0, ErrCollectorOffNeedsLimit
	}
	return debug.SetGCPercent(percent), nil
}
