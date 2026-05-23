package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TestActivator_UnentitledDepDoesNotFailUngated covers an install whose
// policy marks a plugin ungated and grants nothing else: that plugin declares
// a peer the policy does not grant, so the resolver pulls the peer into the
// closure although nothing grants it. Marking the peer failed would put a
// fresh install into PhaseFailed for a plugin nobody granted, and the plugin
// that named it must still boot.
func TestActivator_UnentitledDepDoesNotFailUngated(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	required := ungatedNames(t, 1)[0]
	depender := &depPlugin{name: required, deps: []PluginDependency{{Name: "gamma"}}}
	peer := &depPlugin{name: "gamma"}

	RegisterPlugin(required, func() core.Plugin { return depender })
	RegisterPlugin("gamma", func() core.Plugin { return peer })

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(required), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !depender.started {
		t.Errorf("%s is ungated and must start even when its dependency is not granted", required)
	}
	if peer.started {
		t.Error("gamma is not granted and must not start")
	}

	for _, s := range a.Status().Plugins {
		if s.Name == "gamma" && s.Phase == PhaseFailed {
			t.Errorf("ungranted dependency reported as failed: reason=%q", s.Reason)
		}
	}
}

// TestActivator_EntitledDepIsAutoAdded confirms the skip above is narrow: a
// dependency the license does cover still gets pulled in and started even when
// LYEVE_PLUGINS never named it.
func TestActivator_EntitledDepIsAutoAdded(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	alpha := &depPlugin{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	base := &depPlugin{name: "base"}

	RegisterPlugin("alpha", func() core.Plugin { return alpha })
	RegisterPlugin("base", func() core.Plugin { return base })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha", "base"), "alpha")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !base.started {
		t.Error("base is entitled and required by alpha: it must be auto-added and started")
	}
	if !alpha.started {
		t.Error("alpha was requested and must start")
	}
}
