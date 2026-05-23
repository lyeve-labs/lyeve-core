package plugin

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// CapPolicy is a ceiling intersected with the plugin's declared caps. For a
// plain-registered plugin (core.CapAll) the effective set equals the entry
// the build's table holds.
func TestCapPolicy_CeilingForRegisteredPlugin(t *testing.T) {
	restoreCapTable(t)
	const name = "cron"
	entry := core.CapDBWrite | core.CapRawDB | core.CapRoutes
	SetCapPolicy(map[string]core.Capability{name: entry})
	RegisterPlugin(name, func() core.Plugin { return nil })
	defer UnregisterPlugin(name)
	if got := CapPolicy(name); got != entry {
		t.Errorf("CapPolicy(%q) = %v, want %v", name, got, entry)
	}
}

// An unlisted, unregistered plugin falls back to PluginCaps (0).
func TestCapPolicy_UnlistedFallsBack(t *testing.T) {
	if got := CapPolicy("zzz-nonexistent-plugin"); got != 0 {
		t.Errorf("CapPolicy(unlisted) = %v, want 0", got)
	}
}

// The engine carries no table of its own, so a plugin a build lists nowhere
// runs with what it declared, and with nothing when it declared nothing.
func TestCapPolicy_NoTableGrantsOnlyTheDeclaration(t *testing.T) {
	restoreCapTable(t)
	SetCapPolicy(nil)
	RegisterPlugin("cron", func() core.Plugin { return nil })
	defer UnregisterPlugin("cron")
	RegisterPluginWithCaps("caps-declared", func() core.Plugin { return nil }, core.CapDBRead)
	defer UnregisterPlugin("caps-declared")

	if got := CapPolicy("cron"); got != 0 {
		t.Errorf("CapPolicy(cron) = %v with no table, want 0", got)
	}
	if got := CapPolicy("caps-declared"); got != core.CapDBRead {
		t.Errorf("CapPolicy(caps-declared) = %v with no table, want the declaration", got)
	}
}
