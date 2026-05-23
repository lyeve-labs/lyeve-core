package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// lockoutPlugin stands in for a plugin that counts failed attempts in memory
// and wants somewhere durable to keep the result.
type lockoutPlugin struct {
	*fakePlugin
	got   core.CacheBackend
	calls int
}

func (p *lockoutPlugin) SetRateLimiterCache(cache core.CacheBackend) {
	p.got = cache
	p.calls++
}

type nopBackend struct{}

func (nopBackend) Get(context.Context, string) ([]byte, error)              { return nil, nil }
func (nopBackend) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (nopBackend) Delete(context.Context, string) error                     { return nil }
func (nopBackend) Flush(context.Context) error                              { return nil }

// TestWireAuthCache_ReachesEveryConsumer pins the wiring the runtime calls
// after activation: every running plugin that keeps lockout state gets the
// backend exactly once, and a plugin that keeps none is left alone.
func TestWireAuthCache_ReachesEveryConsumer(t *testing.T) {
	t.Cleanup(resetPluginRegistry)

	locks := &lockoutPlugin{fakePlugin: newFakePlugin("alpha")}
	blocks := &lockoutPlugin{fakePlugin: newFakePlugin("beta")}
	plain := newFakePlugin("gamma")

	RegisterPlugin("alpha", func() core.Plugin { return locks })
	RegisterPlugin("beta", func() core.Plugin { return blocks })
	RegisterPlugin("gamma", func() core.Plugin { return plain })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha", "beta", "gamma"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	backend := nopBackend{}
	if wired := a.WireAuthCache(backend); wired != 2 {
		t.Fatalf("wired %d plugins; want the two that keep lockout state", wired)
	}

	for name, p := range map[string]*lockoutPlugin{"alpha": locks, "beta": blocks} {
		if p.calls != 1 {
			t.Errorf("%s: setter called %d times; want once", name, p.calls)
		}
		if p.got != core.CacheBackend(backend) {
			t.Errorf("%s: got a different backend than the one handed over", name)
		}
	}
}

// An install with no cache backend has nothing to hand over, and the limiters
// have to keep counting in memory rather than take a nil.
func TestWireAuthCache_NoBackendIsNotHandedOver(t *testing.T) {
	t.Cleanup(resetPluginRegistry)

	locks := &lockoutPlugin{fakePlugin: newFakePlugin("alpha")}
	RegisterPlugin("alpha", func() core.Plugin { return locks })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if wired := a.WireAuthCache(nil); wired != 0 {
		t.Fatalf("wired %d plugins with no backend; want 0", wired)
	}
	if locks.calls != 0 {
		t.Errorf("setter called %d times with no backend; want none", locks.calls)
	}
}
