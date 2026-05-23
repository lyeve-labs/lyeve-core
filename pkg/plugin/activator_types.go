package plugin

import (
	"log/slog"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// PluginPhase tracks a plugin's lifecycle state for introspection.
type PluginPhase string

const (
	// PhaseRegistered indicates a plugin is compiled in but never activated.
	PhaseRegistered PluginPhase = "registered" // compiled in, never activated
	// PhaseLazy indicates a lazy-started plugin that has not been activated.
	PhaseLazy PluginPhase = "lazy" // lazy-started. Not yet activated
	// PhaseStarting indicates a plugin is in the process of starting.
	PhaseStarting PluginPhase = "starting"
	// PhaseRunning indicates a plugin is active and running.
	PhaseRunning PluginPhase = "running"
	// PhaseFailed indicates a plugin encountered an error during startup.
	PhaseFailed PluginPhase = "failed"
	// PhaseStopping indicates a plugin is being shut down.
	PhaseStopping PluginPhase = "stopping"
	// PhaseStopped indicates a plugin has completed shutdown.
	PhaseStopped PluginPhase = "stopped"
)

// PluginStatus is one row in PluginStatusReport. Every plugin compiled into
// the binary appears here regardless of whether it activated.
type PluginStatus struct {
	Name       string      `json:"name"`
	Compiled   bool        `json:"compiled"`
	Entitled   bool        `json:"entitled"`
	Requested  bool        `json:"requested"`
	Active     bool        `json:"active"`
	Phase      PluginPhase `json:"phase"`
	Version    string      `json:"version,omitempty"` // semver from Versioned interface
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	StoppedAt  *time.Time  `json:"stopped_at,omitempty"`
	LastError  string      `json:"last_error,omitempty"`
	Reason     string      `json:"reason,omitempty"`      // why inactive (when applicable)
	UpgradeURL string      `json:"upgrade_url,omitempty"` // a link the licensing implementation supplies for an inactive plugin
	// Manifest is how the plugin describes itself, normalized, for a plugin
	// that implements core.Describer. Its label is never empty: a plugin that
	// names none is labeled by its name. A plugin without one has no manifest
	// and is listed by its name.
	Manifest *core.PluginManifest `json:"manifest,omitempty"`
	// Routes are the HTTP routes the plugin mounted, for a running plugin
	// that declares any, sorted by pattern then method.
	Routes []PluginRoute `json:"routes,omitempty"`
}

// PluginRoute is one route a running plugin serves, as the status report
// lists it. The group says who may call it.
type PluginRoute struct {
	Method  string          `json:"method"`
	Pattern string          `json:"pattern"`
	Group   core.RouteGroup `json:"group"`
}

// PluginStatusReport is the response shape for the runtime introspection
// endpoint. Consumers read this to see which plugins
// are compiled in, which the license entitles, and which are running.
type PluginStatusReport struct {
	Compiled  []string       `json:"compiled"`            // every registered plugin
	Entitled  []string       `json:"entitled"`            // what the policy entitles, plugins or not
	Requested []string       `json:"requested,omitempty"` // LYEVE_PLUGINS env, nil if unset
	Plugins   []PluginStatus `json:"plugins"`             // one row per compiled plugin
}

// activatedPlugin tracks a single plugin's runtime state under the activator.
type activatedPlugin struct {
	plugin    core.Plugin
	startedAt time.Time
	stoppedAt time.Time
	phase     PluginPhase
	lastError error
}

// PluginRoutes bundles a plugin's name with its HTTP route declarations.
// Returned by Activator.CollectedRoutes after activation completes.
type PluginRoutes struct {
	Name   string
	Routes []RouteDecl
}

// Activator drives the plugin lifecycle: resolve the activation set, start
// entitled plugins, expose status, stop on shutdown. One Activator per
// process.
type Activator struct {
	host   core.Host
	logger *slog.Logger

	mu       sync.RWMutex
	resolved bool
	compiled []string
	entitled []string
	ungated  map[string]bool // compiled plugins whose failure fails readiness
	reqEnv   []string        // LYEVE_PLUGINS as parsed; nil = env unset
	active   map[string]*activatedPlugin
	status   map[string]*PluginStatus // every compiled plugin keyed by name
	routes   []PluginRoutes           // collected from plugins implementing RoutesPlugin

	// onRoutesChanged is called under mu whenever the route table changes.
	// Set via SetRoutesChangeCallback. Nil means no listener.
	onRoutesChanged func([]PluginRoutes)

	// flows holds the node and trigger types the running plugins contribute.
	// Built by RebuildFlowRegistry, which flowRebuildMu serializes so two
	// rebuilds cannot interleave their snapshots and notifications.
	flows         *flowRegistry
	flowRebuildMu sync.Mutex

	// statelessOnly is set on an engine with no database. Only a plugin that
	// implements core.StatelessCapable starts. The rest are reported inactive
	// with the reason.
	statelessOnly bool

	// purgesRegistered is set once RegisterTenantPurges has run. The purge
	// registry keeps every handler it is given, so a second pass would run
	// each purge twice.
	purgesRegistered bool

	// erasuresRegistered is set once RegisterSubjectErasures has run, for
	// the same reason: each pass builds new erasers the registry cannot
	// recognize as ones it already holds.
	erasuresRegistered bool
}
