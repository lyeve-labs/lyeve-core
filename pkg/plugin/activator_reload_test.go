package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// plainPlugin reads its configuration per request and has nothing to rebuild.
type plainPlugin struct{ name string }

func (p *plainPlugin) Name() string                           { return p.name }
func (p *plainPlugin) Start(context.Context, core.Host) error { return nil }
func (p *plainPlugin) Stop(context.Context) error             { return nil }

// reloadingPlugin caches configuration at Start and rebuilds it on demand.
type reloadingPlugin struct {
	plainPlugin
	mu       sync.Mutex
	calls    int
	failWith error
}

func (p *reloadingPlugin) ReloadConfig(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.failWith
}

func (p *reloadingPlugin) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func activatorWith(plugins ...core.Plugin) *Activator {
	a := &Activator{active: map[string]*activatedPlugin{}}
	for _, p := range plugins {
		a.active[p.Name()] = &activatedPlugin{plugin: p}
	}
	return a
}

func TestActivator_ReloadConfigOnlyAsksPluginsThatCache(t *testing.T) {
	// A plugin reading its configuration per request already serves the new
	// value, so there is nothing to ask it to do.
	reloading := &reloadingPlugin{plainPlugin: plainPlugin{name: "gamma"}}
	a := activatorWith(reloading, &plainPlugin{name: "beta"})

	reloaded, errs := a.ReloadConfig(context.Background())
	assert.Equal(t, 1, reloaded)
	assert.Empty(t, errs)
	assert.Equal(t, 1, reloading.count())
}

func TestActivator_ReloadConfigKeepsGoingAfterAFailure(t *testing.T) {
	// A bad value in one plugin's configuration must not take the process down
	// or stop the others from picking up their own changes.
	broken := &reloadingPlugin{plainPlugin: plainPlugin{name: "broken"}, failWith: errors.New("bad api key")}
	healthy := &reloadingPlugin{plainPlugin: plainPlugin{name: "healthy"}}
	a := activatorWith(broken, healthy)

	reloaded, errs := a.ReloadConfig(context.Background())
	assert.Equal(t, 1, reloaded, "the healthy plugin still reloaded")
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "broken", "the failure names the plugin")
	assert.Contains(t, errs[0].Error(), "bad api key")
}

func TestActivator_ReloadConfigIsRepeatable(t *testing.T) {
	// Saving twice must reach the plugin twice. A reload that only fires once
	// would strand the second change.
	p := &reloadingPlugin{plainPlugin: plainPlugin{name: "gamma"}}
	a := activatorWith(p)

	for i := 1; i <= 3; i++ {
		reloaded, errs := a.ReloadConfig(context.Background())
		require.Empty(t, errs)
		assert.Equal(t, 1, reloaded)
		assert.Equal(t, i, p.count())
	}
}

func TestActivator_ReloadConfigWithNothingActive(t *testing.T) {
	a := activatorWith()
	reloaded, errs := a.ReloadConfig(context.Background())
	assert.Zero(t, reloaded)
	assert.Empty(t, errs)
}

func TestActivator_ReloadConfigSkipsAnEmptySlot(t *testing.T) {
	// A plugin that failed to start leaves its slot behind, and reloading it
	// would panic on a nil.
	a := activatorWith()
	a.active["never-started"] = &activatedPlugin{}
	a.active["nil-slot"] = nil

	assert.NotPanics(t, func() { a.ReloadConfig(context.Background()) })
}

func TestActivator_ReloadConfigDoesNotHoldTheLock(t *testing.T) {
	// A plugin's reload may call back into the activator. Holding the read lock
	// across it is how a deadlock gets built, so the call is made after it is
	// released.
	a := &Activator{active: map[string]*activatedPlugin{}}
	reenters := &reenteringPlugin{name: "reenters", activator: a}
	a.active[reenters.name] = &activatedPlugin{plugin: reenters}

	done := make(chan struct{})
	go func() {
		a.ReloadConfig(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Reported here rather than left to the suite timeout, which would say
		// only that the package hung.
		t.Fatal("ReloadConfig deadlocked: the activator lock was still held when the plugin called back")
	}
	assert.True(t, reenters.reentered, "the plugin reached the activator during its own reload")
}

// reenteringPlugin calls back into the activator from inside ReloadConfig.
type reenteringPlugin struct {
	name      string
	activator *Activator
	reentered bool
}

func (p *reenteringPlugin) Name() string                           { return p.name }
func (p *reenteringPlugin) Start(context.Context, core.Host) error { return nil }
func (p *reenteringPlugin) Stop(context.Context) error             { return nil }

func (p *reenteringPlugin) ReloadConfig(context.Context) error {
	// Any method that takes the activator's read lock will do.
	p.activator.PluginSchema(p.name)
	p.reentered = true
	return nil
}
