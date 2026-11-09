package runtimetune_test

import (
	"math"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The engine's collector endpoint and the role's memory write each check the
// other's setting before making their own. Run against each other, the pair
// must never end with the collector off and no memory limit.
func TestGCAndMemory_NeverEndOffAndUnlimited(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "1073741824"))
	// A round ends this way about once in ten thousand without the lock, and
	// the race detector slows a round tenfold, so the rounds are capped by
	// time as well as by count.
	deadline := time.Now().Add(3 * time.Second)
	for round := 0; round < 100000 && time.Now().Before(deadline); round++ {
		_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryAutomatic})
		if err != nil {
			t.Fatal(err)
		}
		debug.SetGCPercent(100)
		// Both start together, so each check runs before the other's write
		// as often as the scheduler allows.
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-start; _, _ = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryNone}) })
		wg.Go(func() { <-start; _, _ = runtimetune.SetGCPercent(-1) })
		close(start)
		wg.Wait()
		if gogc, _ := runtimetune.GCSettings(); gogc < 0 && debug.SetMemoryLimit(-1) == math.MaxInt64 {
			t.Fatalf("round %d ended with the collector off and no memory limit", round)
		}
	}
}
