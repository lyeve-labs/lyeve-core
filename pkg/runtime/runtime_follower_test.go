package runtime

import (
	"errors"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// The follower ticks on the engine's tracker, so it shows in the goroutine
// views under the engine, and it ends with the tracker at shutdown.
func TestStartFollower_TicksUntilTheTrackerStops(t *testing.T) {
	tracker := engine.NewGoroutineTracker(engine.DefaultTrackerConfig())
	var ticks atomic.Int64
	startFollower(tracker, func() { ticks.Add(1) }, time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for ticks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := ticks.Load(); n < 3 {
		t.Fatalf("ticks = %d after 5s, want at least 3", n)
	}
	if got := tracker.Snapshot().BySource["runtime-follower"]; got != 1 {
		t.Errorf("tracked runtime-follower goroutines = %d, want 1", got)
	}

	if err := tracker.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	stopped := ticks.Load()
	time.Sleep(20 * time.Millisecond)
	if n := ticks.Load(); n != stopped {
		t.Errorf("ticks went from %d to %d after shutdown", stopped, n)
	}
}

// A plugin finds the role on the host from the moment the runtime wires it,
// which both boots do before any plugin starts.
func TestWireRuntimeTuning_HandsTheRoleToTheHost(t *testing.T) {
	cfg := &config.Config{}
	hostBase := enginehost.NewHost(db.NoDatabase(), nil, cfg, hooks.NewRegistry(), "test")
	p, ok := hostBase.(core.RuntimeTuningProvider)
	if !ok {
		t.Fatal("the engine host must implement RuntimeTuningProvider")
	}
	if p.RuntimeTuning() != nil {
		t.Fatal("RuntimeTuning() before wiring = non-nil, want nil")
	}

	tracker := engine.NewGoroutineTracker(engine.DefaultTrackerConfig())
	t.Cleanup(func() { _ = tracker.Shutdown(5 * time.Second) })
	wireRuntimeTuning(hostBase, tracker, runtimetune.Options{Root: fstest.MapFS{}, Config: cfg})

	if p.RuntimeTuning() == nil {
		t.Fatal("RuntimeTuning() after wiring = nil")
	}
	if _, err := p.RuntimeTuning().SetDatabaseMaxConns(5); !errors.Is(err, core.ErrScalingPrimitiveMissing) {
		t.Errorf("SetDatabaseMaxConns without a database = %v, want ErrScalingPrimitiveMissing", err)
	}
}
