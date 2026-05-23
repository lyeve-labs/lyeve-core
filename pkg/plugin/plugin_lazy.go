package plugin

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// LazyPlugin is an optional interface that plugins implement to opt into
// lazy initialization. A lazy plugin is NOT started at boot: instead, the
// activator tracks it as "lazy" and starts it on first use via
// Activator.OnDemandStart.
//
// The plugin's LazyRoutes are mounted at boot time (before Start is called)
// so the router can intercept requests and trigger on-demand activation.
//
//	COMPILED & ENTITLED & REQUESTED -> if LazyPlugin.IsLazy() -> lazy
//	                                -> otherwise                -> started
type LazyPlugin interface {
	core.Plugin

	// IsLazy reports whether this plugin should be lazy-started.
	// Return true to defer Start() until the first request hits
	// one of the plugin's lazy routes.
	IsLazy() bool

	// LazyRoutes returns the plugin's HTTP route declarations without
	// calling Start() first. The activator collects these at resolve time
	// so lazy routes are mounted immediately: hitting a lazy route triggers
	// OnDemandStart.
	LazyRoutes() []RouteDecl
}

// ReadinessReporter is an optional interface that plugins implement to
// participate in the plugin readiness health probe. Plugins return nil
// from Ready() when fully initialized and ready to serve requests.
//
// This is orthogonal to lazy loading: a lazy plugin that has been triggered
// and is still starting should return an error from Ready(), which makes
// the readiness probe return 503 until the lazy plugin comes online.
type ReadinessReporter interface {
	core.Plugin

	// Ready returns nil when the plugin is fully initialized and ready
	// to serve requests. Returns an error describing the initialization
	// state when not yet ready.
	Ready() error
}
