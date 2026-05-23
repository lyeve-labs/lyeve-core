// Adversarial race tests for the plugin activation lifecycle (Activator).
// They stress every public method under concurrent goroutines with -race
// enabled.
//
// Run (single count to avoid global registry conflicts across runs):
//
//	go test -race -count=1 -timeout 120s -run 'TestRace_' ./pkg/plugin/
//
// NOTE: These tests do NOT use t.Parallel(). They rely on the global
// plugin registry, and the race detector's internal goroutines provide
// the concurrent access surface. Running -count=N still works: each
// run serializes (passed, not parallel tests).
//
// The race detector reports only what a run executes, so a path no case calls
// reads as clean. A method that reads activator state needs a case here to be
// checked at all.
package plugin

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Race #1: Concurrent Resolve + Start + Status

func TestRace_ConcurrentResolveStartStatus(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	names := []string{"rp-0", "rp-1", "rp-2", "rp-3", "rp-4", "rp-5", "rp-6", "rp-7", "rp-8", "rp-9"}
	for _, n := range names {
		p := newFakePlugin(n)
		RegisterPlugin(n, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)

	const rounds = 100
	var wg sync.WaitGroup
	for r := 0; r < rounds; r++ {
		wg.Add(3)

		feat := []string{"rp-0", "rp-2", "rp-4", "rp-6", "rp-8"}

		go func() {
			defer wg.Done()
			a.Resolve(grants(feat...), "")
		}()

		go func() {
			defer wg.Done()
			_ = a.Start(context.Background())
		}()

		go func() {
			defer wg.Done()
			_ = a.Status()
		}()

		wg.Wait()
	}
}

// Race #2: Concurrent Start and Stop
//
// This test avoids the nil-plugin path by ensuring all plugins start
// successfully before concurrent Stop calls.

func TestRace_ConcurrentStartStop(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	names := []string{"cs-0", "cs-1", "cs-2", "cs-3", "cs-4"}
	for _, n := range names {
		p := newFakePlugin(n)
		RegisterPlugin(n, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(names...), "")
	_ = a.Start(context.Background())

	ctx := context.Background()
	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Stop(ctx)
		}()
	}
	wg.Wait()
}

// Race #3: Status readers while the route table is re-collected

func TestRace_ConcurrentStatusReaders(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	for _, n := range names {
		p := newFakePlugin(n)
		RegisterPlugin(n, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(names...), "")
	_ = a.Start(context.Background())

	const readers = 50
	const writers = 10
	var wg sync.WaitGroup

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Status()
			_ = a.CollectedRoutes()
		}()
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.RecollectRoutes()
		}()
	}

	wg.Wait()
}

// Race #4: Concurrent Resolve calls

func TestRace_ConcurrentResolves(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	const numPlugins = 20
	for i := 0; i < numPlugins; i++ {
		name := fmt.Sprintf("rr-%d", i)
		p := newFakePlugin(name)
		RegisterPlugin(name, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)

	const N = 50
	var wg sync.WaitGroup

	for i := 0; i < N; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if idx%2 == 0 {
				a.Resolve(grants(), "")
			} else {
				features := []string{"rr-0", fmt.Sprintf("rr-%d", idx%numPlugins)}
				a.Resolve(grants(features...), "")
			}
		}()
	}
	wg.Wait()
}

// Race #5: MFAStore and RiskAssessor under concurrent access

func TestRace_MFAAndRiskConcurrent(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	for _, n := range []string{"mfa", "device-fingerprint", "alpha", "gamma"} {
		p := newFakePlugin(n)
		RegisterPlugin(n, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("mfa", "device-fingerprint", "alpha", "gamma"), "")
	_ = a.Start(context.Background())

	const N = 30
	var wg sync.WaitGroup

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.MFAStore()
			_ = a.RiskAssessor()
			_ = a.Status()
		}()
	}

	wg.Wait()
}

// Race #6: recordPhase / recordRunning interleaving (via concurrent Start)

func TestRace_ConcurrentStartCalls(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	names := make([]string, 10)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("sc-%d", i)
		names[i] = name
		p := newFakePlugin(name)
		RegisterPlugin(name, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(names...), "")

	ctx := context.Background()
	const N = 30
	var wg sync.WaitGroup

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Start(ctx)
		}()
	}

	for i := 0; i < N/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Status()
		}()
	}

	wg.Wait()
}

// Race #7: parseRequested, pure function sanity check

func TestRace_ParseRequestedConcurrent(t *testing.T) {
	const N = 200
	var wg sync.WaitGroup

	for i := 0; i < N; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			switch idx % 5 {
			case 0:
				_ = parseRequested("")
			case 1:
				_ = parseRequested("alpha,gamma,delta")
			case 2:
				_ = parseRequested("  alpha ,  , gamma  ")
			case 3:
				_ = parseRequested("a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p")
			case 4:
				_ = parseRequested(",,")
			}
		}()
	}
	wg.Wait()
}

// Stop with a failed entry: Start records a failed plugin with a nil .plugin
// (recordPhase, recordFailure), and Stop must skip it rather than call
// ap.plugin.Stop(ctx).

func TestStop_NilPluginDeref(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	p := newFakePlugin("crash")
	p.startErr = fmt.Errorf("simulated failure")
	RegisterPlugin("crash", func() core.Plugin { return p })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("crash"), "")
	_ = a.Start(context.Background())
	_ = a.Stop(context.Background()) // must not panic on the failed entry
}

// TestRace_ReloadWhileStarting races ReloadPlugin against a start of the same
// plugin. ReloadPlugin must read ap.plugin and ap.phase under the lock,
// because recordRunning writes both while holding it. The race detector
// reports only what a run executes, so this path needs a case of its own.
func TestRace_ReloadWhileStarting(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	names := []string{"rl-0", "rl-1", "rl-2"}
	for _, n := range names {
		p := newFakePlugin(n)
		RegisterPlugin(n, func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(names...), "")
	_ = a.Start(context.Background())

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, n := range names {
			wg.Add(2)
			go func(n string) { defer wg.Done(); _, _ = a.ReloadPlugin(ctx, n) }(n)
			go func(n string) { defer wg.Done(); _ = a.OnDemandStart(ctx, n) }(n)
		}
	}
	wg.Wait()

	// The detector is the primary oracle here, but a storm that loses a
	// plugin's row, or leaves one with no phase at all, is a bug the detector
	// cannot see. Every name must still be accounted for afterwards.
	report := a.Status()
	seen := make(map[string]PluginPhase, len(names))
	for _, st := range report.Plugins {
		seen[st.Name] = st.Phase
	}
	for _, n := range names {
		phase, ok := seen[n]
		if !ok {
			t.Errorf("plugin %q lost its status row under concurrent reload and start", n)
			continue
		}
		if phase == "" {
			t.Errorf("plugin %q ended with no phase", n)
		}
	}
}
