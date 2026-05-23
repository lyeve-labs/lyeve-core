package plugin

import (
	"fmt"
	"sort"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

var (
	pluginMu       sync.RWMutex
	pluginRegistry = map[string]core.PluginFactory{}
	pluginCaps     = map[string]core.Capability{}
)

// RegisterPlugin records a plugin factory under name. Intended to be called
// from a plugin package's init(): once the plugin's source is imported, the
// init() runs and the plugin appears in the registry.
//
// Registration is build-time. Activation is runtime. A plugin may be
// registered but not activated (no license, not requested, etc.).
//
// A plugin registered here declares no capabilities. It runs with what the
// host capability policy grants its name, and with none when the policy has
// no entry for it, because CapPolicy fails closed. A plugin declares what it
// needs through RegisterPluginWithCaps.
//
// Panics on duplicate name, empty name, or nil factory. These are
// build-time misconfigurations that should fail loud at process startup,
// not silently produce wrong behavior.
func RegisterPlugin(name string, factory core.PluginFactory) {
	if name == "" {
		panic("core.RegisterPlugin: name must be non-empty")
	}
	if factory == nil {
		panic("core.RegisterPlugin: factory must be non-nil for plugin " + name)
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if _, exists := pluginRegistry[name]; exists {
		panic(fmt.Sprintf("core.RegisterPlugin: duplicate registration for %q", name))
	}
	pluginRegistry[name] = factory
}

// RegisterPluginWithCaps records a plugin factory with the capabilities the
// plugin declares it needs. The engine wraps the plugin's Host with a
// ScopedHost that denies any method whose required caps are outside the
// grant, with ErrCapDenied or a stub that does nothing. A host policy entry
// for the name is the ceiling, and the declaration only narrows it. With no
// policy entry, the declaration is the grant (see CapPolicy).
//
// Panics on the same conditions as RegisterPlugin: duplicate name,
// empty name, or nil factory.
func RegisterPluginWithCaps(name string, factory core.PluginFactory, caps core.Capability) {
	RegisterPlugin(name, factory)
	pluginMu.Lock()
	pluginCaps[name] = caps
	pluginMu.Unlock()
}

// PluginCaps returns the registered capabilities for a plugin. Returns
// core.CapAll when the plugin was registered via RegisterPlugin (no explicit
// caps), meaning its own declaration narrows no policy ceiling, or 0 when the
// plugin is not registered. The grant the engine enforces is CapPolicy's.
func PluginCaps(name string) core.Capability {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	caps, ok := pluginCaps[name]
	if !ok {
		if _, registered := pluginRegistry[name]; registered {
			return core.CapAll
		}
		return 0
	}
	return caps
}

// pluginCapsExplicit reports the caps a plugin declared via
// RegisterPluginWithCaps and whether it declared any. Used by CapPolicy to fail
// closed for plugins with no host policy entry.
func pluginCapsExplicit(name string) (core.Capability, bool) {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	caps, ok := pluginCaps[name]
	return caps, ok
}

// PluginCapsEntry is one plugin's capability grant. Caps is what CapPolicy
// grants the name, which is the set the plugin's ScopedHost enforces. Explicit
// is true when the host policy names the plugin or the plugin declared caps
// through RegisterPluginWithCaps. When it is false, Caps is 0, because
// CapPolicy fails closed for a plugin that neither the policy nor the plugin
// itself scoped.
type PluginCapsEntry struct {
	Name     string
	Caps     core.Capability
	Explicit bool
}

// AllPluginCaps returns the capability grant of every registered plugin,
// sorted by name. Each Caps is CapPolicy's value for the name, so the boot
// audit reports what a plugin can reach rather than what it asked for. A
// plugin that neither the host policy nor its own declaration scopes reports
// 0 with Explicit false.
//
// Callers use this at boot to audit the grants and to enforce strict mode
// (fail boot when any plugin lacks explicit caps).
func AllPluginCaps() []PluginCapsEntry {
	pluginMu.RLock()
	out := make([]PluginCapsEntry, 0, len(pluginRegistry))
	for name := range pluginRegistry {
		_, selfDeclared := pluginCaps[name]
		out = append(out, PluginCapsEntry{Name: name, Explicit: selfDeclared})
	}
	pluginMu.RUnlock()

	// CapPolicy takes pluginMu itself. A read lock taken twice deadlocks once
	// a writer queues between the two acquisitions, so the table and the
	// grants are read after the snapshot releases it.
	for i := range out {
		if _, listed := capPolicyEntry(out[i].Name); listed {
			out[i].Explicit = true
		}
		out[i].Caps = CapPolicy(out[i].Name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegisteredPlugins returns the sorted list of plugin names currently
// registered. Used by the runtime's introspection endpoint to report which
// plugins are compiled into this binary.
func RegisteredPlugins() []string {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	out := make([]string, 0, len(pluginRegistry))
	for name := range pluginRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// pluginFactory looks up a registered factory. Internal helper for Activate.
func pluginFactory(name string) (core.PluginFactory, bool) {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	f, ok := pluginRegistry[name]
	return f, ok
}

// ErrPluginNotFound is returned when a name matches no registered plugin.
//
// Exported so the HTTP layer can answer 404 for it. httpx.StoreStatusFor reads
// an unrecognized error as a store failure and would answer 503, reporting the
// service as unavailable when the plugin is simply not installed.
var ErrPluginNotFound = fmt.Errorf("plugin not found in registry")

// LookupPlugin returns the registered factory for the named plugin.
// Returns nil if no plugin with that name is registered. This is the
// public entry point for code that needs access to the registry at
// runtime, such as resolving a declared plugin dependency.
func LookupPlugin(name string) core.PluginFactory {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	return pluginRegistry[name]
}

// ReplacePlugin atomically replaces the factory for name. If name is not
// currently registered, it is added. Returns the previous factory (nil if
// name was not registered). This is the hot-reload entry point: a dev-mode
// builder compiles a new .so, extracts the factory symbol, and calls
// ReplacePlugin to swap it into the registry without a full server restart.
//
// Panics on empty name or nil factory to match RegisterPlugin semantics.
func ReplacePlugin(name string, factory core.PluginFactory) core.PluginFactory {
	if name == "" {
		panic("core.ReplacePlugin: name must be non-empty")
	}
	if factory == nil {
		panic("core.ReplacePlugin: factory must be non-nil for plugin " + name)
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	prev := pluginRegistry[name]
	pluginRegistry[name] = factory
	return prev
}

// UnregisterPlugin removes name from the registry. Returns true if the
// plugin was registered. Safe to call on a name that is not registered.
// Used by hot-reload to clean up after a plugin fails to load or when
// the dev watcher detects a deleted plugin directory.
func UnregisterPlugin(name string) bool {
	if name == "" {
		return false
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if _, exists := pluginRegistry[name]; !exists {
		return false
	}
	delete(pluginRegistry, name)
	delete(pluginCaps, name)
	return true
}

// resetPluginRegistry clears the registry. Test-only. Never called from
// production code paths.
//
//lint:ignore U1000 used in _test.go, but staticcheck misses them due to import cycle in audit_integration_test.go
func resetPluginRegistry() {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	pluginRegistry = map[string]core.PluginFactory{}
	pluginCaps = map[string]core.Capability{}
}
