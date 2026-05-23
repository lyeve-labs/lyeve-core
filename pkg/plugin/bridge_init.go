package plugin

import "github.com/lyeve-labs/lyeve-core/pkg/core"

func init() {
	// Registration is wired through a setter, not a var, because it is the
	// only bridge a plugin calls during init. See core.SetPluginRegistrar.
	core.SetPluginRegistrar(RegisterPlugin, RegisterPluginWithCaps)
	core.PluginMigrate = PluginMigrate
	core.PluginAppliedVersions = PluginAppliedVersions
	core.PluginCaps = PluginCaps
	core.AllPluginCaps = func() []core.PluginCapsEntry {
		entries := AllPluginCaps()
		out := make([]core.PluginCapsEntry, len(entries))
		for i, e := range entries {
			out[i] = core.PluginCapsEntry{
				Name:     e.Name,
				Caps:     e.Caps,
				Explicit: e.Explicit,
			}
		}
		return out
	}
	core.RegisteredPlugins = RegisteredPlugins
	core.ResetPluginRegistry = resetPluginRegistry
	core.LookupPlugin = LookupPlugin
	core.PluginMigrateRollback = PluginMigrateRollback
}
