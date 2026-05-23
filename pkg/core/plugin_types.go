package core

import "context"

// Plugin is the contract every plugin implements. A plugin is registered
// via RegisterPlugin from its package's init() function. The runtime decides
// which plugins to activate from the entitlement the license verifier grants
// and the LYEVE_PLUGINS env.
//
// The activation matrix is:
//
//	COMPILED  &  ENTITLED  &  REQUESTED  =  ACTIVE
//
//	COMPILED - plugin imported by this binary's main package (init() ran)
//	ENTITLED - the configured license verifier grants the name, or it needs no license
//	REQUESTED - plugin name appears in LYEVE_PLUGINS env (or env unset = all entitled)
//
// Plugins that aren't in all three sets remain registered but inert.
type Plugin interface {
	// Name returns the plugin's identifier, such as "my-plugin". The
	// entitlement check and LYEVE_PLUGINS both name the plugin by it.
	Name() string

	// Start initializes the plugin against the host and begins serving. It
	// should return when the plugin is ready, or an error if startup fails.
	// Long-lived workers should be spawned as goroutines from inside Start.
	//
	// The provided context is the runtime's lifetime. When it cancels, the
	// runtime calls Stop. Plugins should also propagate ctx to their workers.
	Start(ctx context.Context, host Host) error

	// Stop gracefully drains the plugin. The runtime calls this on shutdown.
	// Plugins should release resources, finish in-flight work, and return.
	Stop(ctx context.Context) error
}

// PluginFactory constructs a fresh Plugin instance. The runtime calls this
// once per process during Activate. The returned Plugin is owned by the
// runtime for its remaining lifetime.
type PluginFactory func() Plugin

// PluginDependency describes a required peer plugin.
type PluginDependency struct {
	Name       string `json:"name"`
	MinVersion string `json:"min_version,omitempty"`
	MaxVersion string `json:"max_version,omitempty"`
}

// Depender is an optional interface a Plugin can implement to declare
// its required peer plugins.
type Depender interface {
	Dependencies() []PluginDependency
}

// Capability is a bitmask describing what a plugin is allowed to do.
// Plugins declare capabilities at registration time via RegisterPluginWithCaps.
// The engine wraps each plugin's Host with a ScopedHost that checks the
// granted caps before delegating to the real engine host. The grant is the
// host capability policy's entry for the plugin narrowed by its declaration,
// or the declaration alone when the policy has no entry.
//
// CapDBWrite implies CapDBRead.
// A plugin that registers via RegisterPlugin declares nothing, so it gets the
// policy's entry for its name, or no capabilities when there is none.
type Capability uint32

const (
	// CapDBRead grants read-only database access via Querier and QuerierRO.
	CapDBRead Capability = 1 << iota // read-only database access (Querier, QuerierRO)
	// CapDBWrite grants read-write database access. Implies CapDBRead.
	CapDBWrite // read-write database access (implies DBRead)
	// CapRawDB grants access to *sql.DB for migration use. PRIVILEGED.
	CapRawDB // access to *sql.DB for Migrate (PRIVILEGED)
	// CapConfigSecret allows reading secret config keys.
	CapConfigSecret // read secret config keys
	_               // reserved, so every capability after it has a fixed value
	// CapHooks allows publishing and subscribing to hook events.
	CapHooks // publish/subscribe to hook events
	// CapRoutes allows registering HTTP routes.
	CapRoutes // register HTTP routes
	// CapAdmin grants access to admin-only endpoints. PRIVILEGED.
	CapAdmin // admin-only endpoints (PRIVILEGED)
	// CapSchema grants schema DDL operations (Apply, Delete, ApplyPending).
	CapSchema // schema DDL operations (Apply/Delete/ApplyPending)
	// CapFlowRegistry grants FlowRegistry(): the node and trigger types every
	// started plugin contributes. Only the plugin that runs flows reads it.
	// Contributing types needs no grant, because a contribution is the plugin
	// offering something and the node runs under the plugin's own host.
	CapFlowRegistry // read the flow registry (FlowRegistry)
	// CapMetrics grants MetricsGatherer(): the engine's own registry, with
	// every tenant's request series in it. Only a plugin that exports the
	// instance's metrics reads it.
	CapMetrics // read the engine's metrics registry (MetricsGatherer)
	// CapConfigSectionsRead grants ConfigSections(): every plugin's
	// configuration section, which exports that plugin's secrets under a
	// sealer the caller supplies and applies a bundle to its tables. Only the
	// plugin that assembles a configuration bundle reads it. Registering a
	// section needs no grant, because a registration offers the plugin's own
	// configuration.
	CapConfigSectionsRead // read every registered configuration section (ConfigSections)

	// CapAll names every capability. PluginCaps reports it for a plugin that
	// registered via RegisterPlugin, because a declaration of nothing narrows
	// no policy ceiling. It is not that plugin's grant (see plugin.CapPolicy).
	CapAll Capability = CapDBRead | CapDBWrite | CapRawDB | CapConfigSecret |
		CapHooks | CapRoutes | CapAdmin | CapSchema | CapFlowRegistry | CapMetrics |
		CapConfigSectionsRead
)

// Has reports whether this capability set includes c.
// CapDBWrite implies CapDBRead.
func (c Capability) Has(need Capability) bool {
	if c&need != 0 {
		return true
	}
	if need == CapDBRead && c&CapDBWrite != 0 {
		return true
	}
	return false
}
