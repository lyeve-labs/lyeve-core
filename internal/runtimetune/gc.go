// Package runtimetune holds the process's runtime and scaling settings: the
// scheduler's thread count, Go's soft memory limit, the garbage collector's
// target and the primary database pool's size. It serves them to plugins as
// core.RuntimeTuning, follows the cgroup's limits while the process runs, and
// keeps the rule the engine's own garbage collector endpoint shares.
package runtimetune

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"sync"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// gcMu makes each check of the collector against the memory limit, and the
// change it allows, one step. The engine's endpoint and the role each check
// the other's setting, and without it both checks can pass before either
// change lands, which leaves the collector off with no limit.
var gcMu sync.Mutex

// ErrCollectorOffNeedsLimit refuses to turn the garbage collector off without
// a memory limit below the container's or the machine's memory. Without one
// nothing bounds the heap short of the kernel's OOM kill.
var ErrCollectorOffNeedsLimit = errors.New("gogc below zero turns the garbage collector off, which needs a memory limit below the container's or the machine's memory")

// ErrGCPercentNeedsLimit refuses a collector percentage above
// core.MaxGCPercent without a memory limit below the container's or the
// machine's memory, by the same rule.
var ErrGCPercentNeedsLimit = fmt.Errorf("gogc above %d needs a memory limit below the container's or the machine's memory", core.MaxGCPercent)

// ErrLimitNeededByCollector refuses to remove the memory limit while the
// garbage collector is off, by the same rule seen from the memory side. A
// limit at or above the container's or the machine's memory removes it.
var ErrLimitNeededByCollector = errors.New("the garbage collector is off, so the memory limit cannot be removed until it is turned back on")

// ErrLimitNeededByGCPercent refuses to remove the memory limit while the
// collector percentage is above core.MaxGCPercent.
var ErrLimitNeededByGCPercent = fmt.Errorf("the garbage collector percentage is above %d, so the memory limit cannot be removed until it is %d or less", core.MaxGCPercent, core.MaxGCPercent)

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
// replaced. Without a memory limit below the container's or the machine's
// memory, a negative percentage is refused with ErrCollectorOffNeedsLimit
// and one above core.MaxGCPercent with ErrGCPercentNeedsLimit, and the
// percentage in force stays.
func SetGCPercent(percent int) (int, error) {
	return setGCPercent(percent, physicalMemory(os.DirFS("/")))
}

// setGCPercent is SetGCPercent against the memory physical, the container's
// or the machine's, with 0 when neither could be read.
func setGCPercent(percent int, physical int64) (int, error) {
	gcMu.Lock()
	defer gcMu.Unlock()
	if noEffectiveLimit(debug.SetMemoryLimit(-1), physical) {
		switch {
		case percent < 0:
			return 0, ErrCollectorOffNeedsLimit
		case percent > core.MaxGCPercent:
			return 0, ErrGCPercentNeedsLimit
		}
	}
	return debug.SetGCPercent(percent), nil
}

// noEffectiveLimit reports whether the soft limit bounds nothing: none is
// set, or it is at or above the memory physical, where the kernel's OOM kill
// comes first. physical is 0 when it could not be read.
func noEffectiveLimit(limit, physical int64) bool {
	return limit == math.MaxInt64 || (physical > 0 && limit >= physical)
}

// physicalMemory is the container's memory limit under root, else the
// machine's memory, else 0.
func physicalMemory(root fs.FS) int64 {
	if _, limit := config.ReadCgroup(root).Limits(root); limit > 0 {
		return limit
	}
	return config.MachineMemory(root)
}
