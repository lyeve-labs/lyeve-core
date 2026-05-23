package core

import (
	"context"
	"net/http"
)

// The custom route slot.
//
// A plugin's routes are declared once, at start, as patterns. A path a
// tenant chooses at run time, such as the URL a flow answers, cannot be one
// of them. The engine keeps one slot for it on the API router: when no
// registered route matches a request, the router's not-found handler asks
// the provider before it answers 404. A registered route therefore always
// wins, and a custom path can never shadow an engine or plugin route, only
// be shadowed by one added later.
//
// The not-found handler runs on the outer router, outside the /api/v1
// group, so the request has passed the security headers, the path
// sanitizer, the body ceiling, the per-address limiter and the allowlist,
// and nothing else: no credentials were read and no tenant was resolved.
// A provider serves a public surface. It finds the tenant from the path it
// owns, never from the request, and acquires the tenant's connection
// itself.
//
// Usage in the plugin:
//
//	var _ core.CustomRouteProvider = (*Plugin)(nil)
//
//	func (p *Plugin) CustomRoute(r *http.Request) http.Handler {
//	    return p.endpoints.lookup(r.URL.Path)
//	}

// CustomRouteProvider is implemented by a plugin that serves paths chosen at
// run time. CustomRoute is called on every request no route matched, so it
// must answer from memory. A nil handler leaves the 404.
type CustomRouteProvider interface {
	CustomRoute(r *http.Request) http.Handler
}

// DocumentedEndpoint is one URL a plugin serves for the caller's tenant that
// its route declarations cannot name, listed in the OpenAPI document beside
// the declared routes.
type DocumentedEndpoint struct {
	Method      string // an HTTP method, or ANY for every method
	Path        string // the URL path, with {name} for a path parameter
	Summary     string
	Description string
	Tag         string
	// Group is who may call it, in the route groups' terms.
	Group RouteGroup
	// Feature is the license feature the endpoint needs, empty for none.
	Feature string
	// Example is a request body the reference offers, nil for none.
	Example map[string]any
}

// EndpointDocumenter is implemented by a plugin whose tenants create
// endpoints. DocumentedEndpoints reads the caller's tenant from ctx.
type EndpointDocumenter interface {
	DocumentedEndpoints(ctx context.Context) ([]DocumentedEndpoint, error)
}
