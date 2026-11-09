package runtimetune_test

import (
	"os"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
)

// BenchmarkTick measures one follower tick on this machine's own cgroup and
// /proc, with both the thread count and the soft limit following, which is
// the most a tick reads. The follower runs one a second.
func BenchmarkTick(b *testing.B) {
	if os.Getenv("GOMAXPROCS") != "" || os.Getenv("GOMEMLIMIT") != "" {
		b.Skip("a variable pins the values the tick follows")
	}
	procs := runtime.GOMAXPROCS(0)
	limit := debug.SetMemoryLimit(-1)
	b.Cleanup(func() {
		debug.SetMemoryLimit(limit)
		runtime.SetDefaultGOMAXPROCS()
		if runtime.GOMAXPROCS(0) != procs {
			runtime.GOMAXPROCS(procs)
		}
	})
	tuner := runtimetune.New(runtimetune.Options{Root: os.DirFS("/"), Resolver: config.NewResolver(nil)})
	b.ReportAllocs()
	for b.Loop() {
		tuner.Tick()
	}
}
