package core

// The API router's own paths.
//
// A plugin that lets a tenant choose a URL, such as the path a flow answers,
// has to refuse one the API router already serves: a registered route always
// wins over a custom path, so a path that collides with one is a URL that
// never reaches the plugin. A hand-kept list of reserved prefixes goes stale
// the day a plugin adds a route, so the engine answers from the router it
// built instead, and answers again after a hot reload rebuilds it.
//
// Usage in the plugin:
//
//	if p, ok := host.(core.APIRoutesProvider); ok {
//	    if routes := p.APIRoutes(); routes != nil && routes.Owns(path) {
//	        // refuse the path
//	    }
//	}

// APIRoutes reports which paths the API router's registered routes own.
type APIRoutes interface {
	// Owns reports whether path belongs to a route the engine or a plugin
	// registered on the API router. A path is owned when a registered route
	// matches it for any method, and when it sits under a namespace one
	// opens: the first segment after /api/v1/ (or after /api/, or the first
	// segment outside /api) of any registered pattern. So /api/v1/content
	// owns /api/v1/content/anything, whether or not a registered pattern
	// matches it. A path equal to /api or /api/v1 is owned.
	Owns(path string) bool
}

// APIRoutesRegistrar is implemented by the engine host. The runtime calls it
// once the API router exists, with an answer that follows the router across
// hot reloads.
type APIRoutesRegistrar interface {
	RegisterAPIRoutes(r APIRoutes)
}

// APIRoutesProvider is implemented by the engine host. Nil means the runtime
// has not registered the router yet. A plugin validating a path then refuses
// the prefixes it cannot check rather than accepting them.
type APIRoutesProvider interface {
	APIRoutes() APIRoutes
}
