package plugin

import (
	"slices"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RegisterTenantPurges registers the tenant purges of every plugin compiled
// into the build, through core.TenantPurger, and reports how many plugins
// contributed one. It runs once per activator, and a later call registers
// nothing.
//
// Every compiled plugin counts, not only the running ones. A plugin its
// capability set refuses, or one LYEVE_PLUGINS leaves out, still owns the
// tables an earlier run created, and a tenant delete has to reach them. Each
// purge comes from a fresh instance, because its handler must work on a
// plugin that never starts.
//
// Dependents come before the plugins they depend on, so a handler that finds
// its rows through a table another plugin owns runs while that table still
// holds them.
func (a *Activator) RegisterTenantPurges(dialect string) int {
	a.mu.Lock()
	if a.purgesRegistered {
		a.mu.Unlock()
		return 0
	}
	a.purgesRegistered = true
	a.mu.Unlock()

	registered := 0
	for _, name := range purgeOrder(RegisteredPlugins()) {
		factory, ok := pluginFactory(name)
		if !ok {
			continue
		}
		p, ok := factory().(core.TenantPurger)
		if !ok {
			continue
		}
		purges := p.TenantPurges()
		for _, tp := range purges {
			core.RegisterTenantPurge(dialect, tp)
		}
		if len(purges) > 0 {
			registered++
			a.logger.Debug("tenant purge registered", "plugin", name, "sets", len(purges))
		}
	}
	return registered
}

// purgeOrder is the dependency load order reversed, so each plugin comes
// before the ones it depends on. A plugin the resolution leaves out, through
// a cycle or a missing dependency, follows in name order.
func purgeOrder(compiled []string) []string {
	order := slices.Clone(ResolvePluginDeps(compiled).Order)
	slices.Reverse(order)
	placed := make(map[string]bool, len(order))
	for _, name := range order {
		placed[name] = true
	}
	for _, name := range compiled {
		if !placed[name] {
			order = append(order, name)
		}
	}
	return order
}
