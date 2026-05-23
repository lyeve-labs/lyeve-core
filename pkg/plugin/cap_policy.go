package plugin

import (
	"sort"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// CapPolicy returns the effective capability set the engine grants the named
// plugin, which is what its scoped host enforces. The build decides what each
// plugin may reach, through the table it hands SetCapPolicy, and a plugin's
// own declaration (RegisterPluginWithCaps) can only narrow that grant, so a
// compiled-in plugin cannot escalate by declaring core.CapAll. A plugin with
// no entry fails closed: it gets only the caps it declared via
// RegisterPluginWithCaps, never the implicit core.CapAll, so a plugin someone
// forgets to list cannot silently run with secret, raw database or admin
// access.
func CapPolicy(name string) core.Capability {
	if p, ok := capPolicyEntry(name); ok {
		return p & PluginCaps(name)
	}
	if caps, explicit := pluginCapsExplicit(name); explicit {
		return caps
	}
	return 0
}

// CapPolicyNames returns every plugin name carrying an explicit capability
// grant, sorted. A name here that is not a registered plugin is dead policy.
// A registered plugin missing from here gets only its self-declared caps, and
// none when it declared nothing. A build that links plugins holds its own
// table to them in both directions.
func CapPolicyNames() []string {
	capTableMu.RLock()
	out := make([]string, 0, len(capTable))
	for name := range capTable {
		out = append(out, name)
	}
	capTableMu.RUnlock()
	sort.Strings(out)
	return out
}

var (
	capTableMu sync.RWMutex
	// capTable is the table CapPolicy reads. It is empty until a build hands
	// the engine one through SetCapPolicy, so a build that hands none grants
	// each plugin what it declared and nothing else.
	capTable map[string]core.Capability
)

// SetCapPolicy replaces the table CapPolicy reads with a copy of p. A plugin
// p names gets its entry as the ceiling, which its own declaration can only
// narrow. A plugin p leaves out gets what it declared through
// RegisterPluginWithCaps, and nothing when it declared nothing, so a nil p
// leaves every plugin with its own declaration alone.
//
// The runtime calls it before any plugin starts, with the table the build
// passed. A change after that reaches only the plugins started later.
func SetCapPolicy(p map[string]core.Capability) {
	next := make(map[string]core.Capability, len(p))
	for name, caps := range p {
		next[name] = caps
	}
	capTableMu.Lock()
	capTable = next
	capTableMu.Unlock()
}

// capPolicyEntry is the entry the table in force holds for name.
func capPolicyEntry(name string) (core.Capability, bool) {
	capTableMu.RLock()
	defer capTableMu.RUnlock()
	caps, ok := capTable[name]
	return caps, ok
}
