package runtime

import (
	"context"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// followInterval is how often the follower reads the cgroup's limits again.
// It matches the Go runtime's own cadence for the CPU limit, and a memory
// limit that shrinks is the case that needs an answer before the kernel's.
const followInterval = time.Second

// wireRuntimeTuning builds the runtime settings role over opts, hands it to
// the host and starts the follower on the engine's tracker.
func wireRuntimeTuning(hostBase core.Host, tracker *engine.GoroutineTracker, opts runtimetune.Options) *runtimetune.Tuner {
	t := runtimetune.New(opts)
	if setter, ok := hostBase.(interface{ WithRuntimeTuning(core.RuntimeTuning) }); ok {
		setter.WithRuntimeTuning(t)
	}
	startFollower(tracker, t.Tick, followInterval)
	return t
}

// startFollower calls tick every interval until the tracker shuts down. The
// tick itself decides what follows, so a value fixed by a write and later
// returned to automatic follows again without a restart.
func startFollower(tracker *engine.GoroutineTracker, tick func(), every time.Duration) {
	tracker.GoLongLived("runtime-follower", func(ctx context.Context) {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tick()
			}
		}
	})
}
