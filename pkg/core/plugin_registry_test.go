// Registry-bridge tests. The core package only holds function-pointer hooks,
// and pkg/plugin fills them from its init(). Because pkg/plugin imports core, an
// in-package test can never link it, so these live in the external test
// package and pull the bridge in explicitly below.
package core_test

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	_ "github.com/lyeve-labs/lyeve-core/pkg/plugin" // links the registry bridge
)

type stubPlugin struct{ name string }

func (p *stubPlugin) Name() string                                    { return p.name }
func (p *stubPlugin) Start(ctx context.Context, host core.Host) error { return nil }
func (p *stubPlugin) Stop(ctx context.Context) error                  { return nil }

// TestPluginRegistryBridge_Linked fails loudly if pkg/plugin stops populating
// the hooks. Without it the tests below would silently exercise nothing.
func TestPluginRegistryBridge_Linked(t *testing.T) {
	if core.ResetPluginRegistry == nil {
		t.Fatal("ResetPluginRegistry hook is nil: pkg/plugin init did not run")
	}
	if core.PluginCaps == nil {
		t.Fatal("PluginCaps hook is nil: pkg/plugin init did not run")
	}
	if core.AllPluginCaps == nil {
		t.Fatal("AllPluginCaps hook is nil: pkg/plugin init did not run")
	}
}

// RegisterPluginWithCaps + PluginCaps

func TestRegisterPluginWithCaps(t *testing.T) {
	defer core.ResetPluginRegistry()

	name := "test-plugin-caps"
	factory := func() core.Plugin { return &stubPlugin{name: name} }

	core.RegisterPluginWithCaps(name, factory, core.CapDBRead|core.CapHooks)

	caps := core.PluginCaps(name)
	if !caps.Has(core.CapDBRead) {
		t.Error("expected CapDBRead")
	}
	if !caps.Has(core.CapHooks) {
		t.Error("expected CapHooks")
	}
	if caps.Has(core.CapDBWrite) {
		t.Error("did not expect CapDBWrite")
	}
}

func TestPluginCaps_RegisterPluginWithoutCaps_ReturnsCapAll(t *testing.T) {
	defer core.ResetPluginRegistry()

	name := "undeclared-plugin"
	factory := func() core.Plugin { return &stubPlugin{name: name} }

	core.RegisterPlugin(name, factory)

	caps := core.PluginCaps(name)
	if caps != core.CapAll {
		t.Errorf("PluginCaps should report CapAll for a plugin that declared nothing, got %d", caps)
	}
}

func TestPluginCaps_UnknownPlugin_ReturnsZero(t *testing.T) {
	defer core.ResetPluginRegistry()

	caps := core.PluginCaps("non-existent")
	if caps != 0 {
		t.Errorf("unknown plugin should return 0, got %d", caps)
	}
}

// AllPluginCaps

func TestAllPluginCaps_EmptyRegistry(t *testing.T) {
	core.ResetPluginRegistry()
	defer core.ResetPluginRegistry()

	entries := core.AllPluginCaps()
	if len(entries) != 0 {
		t.Errorf("empty registry should return empty slice, got %d entries", len(entries))
	}
}

func TestAllPluginCaps_MixedExplicitAndImplicit(t *testing.T) {
	defer core.ResetPluginRegistry()

	core.RegisterPlugin("unscoped-plugin", func() core.Plugin { return &stubPlugin{name: "unscoped-plugin"} })
	core.RegisterPluginWithCaps("scoped-plugin", func() core.Plugin { return &stubPlugin{name: "scoped-plugin"} }, core.CapDBRead|core.CapHooks)

	entries := core.AllPluginCaps()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Sorted by name.
	if entries[0].Name != "scoped-plugin" {
		t.Errorf("expected scoped-plugin first (sorted), got %s", entries[0].Name)
	}
	if entries[1].Name != "unscoped-plugin" {
		t.Errorf("expected unscoped-plugin second (sorted), got %s", entries[1].Name)
	}

	// scoped-plugin: explicit
	if !entries[0].Explicit {
		t.Error("scoped-plugin should be Explicit=true")
	}
	if !entries[0].Caps.Has(core.CapDBRead) {
		t.Error("scoped-plugin should have CapDBRead")
	}
	if !entries[0].Caps.Has(core.CapHooks) {
		t.Error("scoped-plugin should have CapHooks")
	}
	if entries[0].Caps.Has(core.CapDBWrite) {
		t.Error("scoped-plugin should NOT have CapDBWrite")
	}

	// unscoped-plugin: neither the host policy nor the plugin scoped it, so the
	// engine grants it nothing and the report must say so.
	if entries[1].Explicit {
		t.Error("unscoped-plugin should be Explicit=false")
	}
	if entries[1].Caps != 0 {
		t.Errorf("unscoped-plugin is granted no capabilities, but the report says %d", entries[1].Caps)
	}
}

func TestAllPluginCaps_ExplicitCapAll(t *testing.T) {
	defer core.ResetPluginRegistry()

	// RegisterPluginWithCaps with CapAll is still Explicit=true.
	core.RegisterPluginWithCaps("full-explicit", func() core.Plugin { return &stubPlugin{name: "full-explicit"} }, core.CapAll)

	entries := core.AllPluginCaps()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !entries[0].Explicit {
		t.Error("explicit CapAll should still be Explicit=true")
	}
	if entries[0].Caps != core.CapAll {
		t.Errorf("expected CapAll, got %d", entries[0].Caps)
	}
}
