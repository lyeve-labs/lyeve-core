package core

import (
	"sync"
	"sync/atomic"
)

// A public route with no limit of its own runs on the general public limit,
// the per-address cap every public route shares. An operator can give one of
// those routes a limit of its own at runtime through a resolver: the public
// limiter asks it on each request to such a route, and a limit it answers
// replaces the general one for that route, higher or lower.
//
// A route that has a limit of its own keeps it whatever a resolver answers,
// and so does every route under /auth. The setting that moves those limits
// is the deployment's, not an administrator's at runtime: many stand between
// a public caller and a credential (sign-in, a token, a device code), and the
// rest were set for what their route costs.

// PublicRouteLimitResolver answers the limit an operator set on a public
// route. endpoint is "METHOD /pattern", as the route was declared, such as
// "GET /api/v1/localization/resolve". It is called on every request to a
// route on the general limit, so it answers from memory.
type PublicRouteLimitResolver interface {
	PublicRouteLimit(endpoint string) (RouteRateLimit, bool)
}

var publicRouteLimits struct {
	mu         sync.Mutex
	resolver   atomic.Pointer[publicRouteLimitEntry]
	generation uint64
}

type publicRouteLimitEntry struct {
	r          PublicRouteLimitResolver
	generation uint64
}

// RegisterPublicRouteLimitResolver installs r and returns the function that
// removes it. One resolver is held at a time: a later registration replaces
// an earlier one, and removing a replaced resolver leaves its successor in
// place, so a plugin restarted in the same process cannot unregister the
// copy that replaced it.
func RegisterPublicRouteLimitResolver(r PublicRouteLimitResolver) func() {
	publicRouteLimits.mu.Lock()
	defer publicRouteLimits.mu.Unlock()
	publicRouteLimits.generation++
	gen := publicRouteLimits.generation
	publicRouteLimits.resolver.Store(&publicRouteLimitEntry{r: r, generation: gen})
	return func() {
		publicRouteLimits.mu.Lock()
		defer publicRouteLimits.mu.Unlock()
		if cur := publicRouteLimits.resolver.Load(); cur != nil && cur.generation == gen {
			publicRouteLimits.resolver.Store(nil)
		}
	}
}

// ResolvePublicRouteLimit answers the operator's limit for a public route on
// the general limit. It reports false when no resolver is registered, when
// the resolver has no limit for the route, or when the limit it holds could
// not be enforced: a rate or burst that is not positive would refuse every
// request rather than pace them.
func ResolvePublicRouteLimit(endpoint string) (RouteRateLimit, bool) {
	cur := publicRouteLimits.resolver.Load()
	if cur == nil || cur.r == nil {
		return RouteRateLimit{}, false
	}
	l, ok := cur.r.PublicRouteLimit(endpoint)
	if !ok || l.Rate <= 0 || l.Burst <= 0 {
		return RouteRateLimit{}, false
	}
	return l, true
}
