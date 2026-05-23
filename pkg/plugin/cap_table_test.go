package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// restoreCapTable puts back the table in force before the test when it ends.
func restoreCapTable(t *testing.T) {
	t.Helper()
	capTableMu.RLock()
	before := capTable
	capTableMu.RUnlock()
	t.Cleanup(func() {
		capTableMu.Lock()
		capTable = before
		capTableMu.Unlock()
	})
}

func capsByName() map[string]PluginCapsEntry {
	out := map[string]PluginCapsEntry{}
	for _, e := range AllPluginCaps() {
		out[e.Name] = e
	}
	return out
}

// A build hands the engine its table and every reader follows it: the grant a
// scoped host enforces, the names the wiring test holds to the registry, and
// the boot audit.
func TestSetCapPolicy_TheBuildsTableIsTheCeiling(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	const onlyInTheEnginesTable = "cron"
	for _, name := range []string{onlyInTheEnginesTable, "caps-table-listed"} {
		p := &hostKeeper{name: name}
		RegisterPlugin(name, func() core.Plugin { return p })
	}
	// A table an earlier caller installed, which the build's replaces whole.
	SetCapPolicy(map[string]core.Capability{onlyInTheEnginesTable: core.CapDBWrite | core.CapRawDB | core.CapRoutes})
	require.NotZero(t, CapPolicy(onlyInTheEnginesTable))
	declared := &hostKeeper{name: "caps-table-declared"}
	RegisterPluginWithCaps(declared.name, func() core.Plugin { return declared }, core.CapDBRead|core.CapHooks)

	table := map[string]core.Capability{
		"caps-table-listed":   core.CapRoutes | core.CapHooks,
		"caps-table-declared": core.CapDBRead,
	}
	SetCapPolicy(table)
	table["caps-table-listed"] = core.CapAll

	assert.Equal(t, core.CapRoutes|core.CapHooks, CapPolicy("caps-table-listed"),
		"the engine keeps its own copy, so a change to the caller's map grants nothing")
	assert.Equal(t, core.CapDBRead, CapPolicy("caps-table-declared"), "a declaration still narrows the entry")
	assert.Zero(t, CapPolicy(onlyInTheEnginesTable), "an entry the build's table leaves out is gone")
	assert.Equal(t, []string{"caps-table-declared", "caps-table-listed"}, CapPolicyNames())

	audit := capsByName()
	assert.True(t, audit["caps-table-listed"].Explicit)
	assert.Equal(t, core.CapRoutes|core.CapHooks, audit["caps-table-listed"].Caps)
	assert.False(t, audit[onlyInTheEnginesTable].Explicit, "the audit reads the table in force")
	assert.Zero(t, audit[onlyInTheEnginesTable].Caps)
}

// With no table at all a plugin is scoped by its own declaration or not at
// all, which the boot audit reports as none.
func TestSetCapPolicy_NilLeavesEachPluginItsDeclaration(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	plain := &hostKeeper{name: "cron"}
	RegisterPlugin(plain.name, func() core.Plugin { return plain })
	declared := &hostKeeper{name: "caps-table-declared"}
	RegisterPluginWithCaps(declared.name, func() core.Plugin { return declared }, core.CapHooks)

	SetCapPolicy(nil)

	assert.Zero(t, CapPolicy(plain.name))
	assert.Equal(t, core.CapHooks, CapPolicy(declared.name))
	assert.Empty(t, CapPolicyNames())
	audit := capsByName()
	assert.False(t, audit[plain.name].Explicit)
	assert.True(t, audit[declared.name].Explicit)
}
