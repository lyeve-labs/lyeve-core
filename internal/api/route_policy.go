package api

import (
	"net/http"
	"strings"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// The readers of the policy a route declares for itself. The engine keeps
// policy only for the routes it serves: a rate limit row, a credential root
// and a session-only pattern. A route of any other owner that declares
// nothing gets none of them, and the document describes it from its method,
// pattern and group.

// DeclaredPublicRateLimits reads the limits public routes declare, keyed the
// way the limiter's Wrap keys a route. The limiter wraps no other group, so a
// limit on another group's route is left out. So is one whose rate or burst
// is not positive, which would refuse the route's traffic instead of pacing
// it. WarnUnmountableRoutes reports both at boot. The routers merge these
// over the engine's own rows, and the runtime reads them to report the
// limits in force.
func DeclaredPublicRateLimits(sets []plugin.PluginRoutes) map[string]apimw.PublicRateLimitConfig {
	var out map[string]apimw.PublicRateLimitConfig
	for _, pr := range sets {
		for _, rd := range pr.Routes {
			if !appliedRateLimit(rd) {
				continue
			}
			if out == nil {
				out = map[string]apimw.PublicRateLimitConfig{}
			}
			out[rd.Method+":"+rd.Pattern] = apimw.PublicRateLimitConfig{Rate: rd.RateLimit.Rate, Burst: rd.RateLimit.Burst}
		}
	}
	return out
}

// appliedRateLimit reports whether the public limiter enforces the limit a
// route declares.
func appliedRateLimit(rd plugin.RouteDecl) bool {
	rl := rd.RateLimit
	return rl != nil && rd.Group == plugin.GroupPublic && rl.Rate > 0 && rl.Burst > 0
}

// sensitiveRoutes answers whether the route a request is addressed to
// declared its bodies sensitive. The pattern comes from the routing tree
// rather than from the request, so a request a middleware refuses before
// routing is judged by the route it named, and its body stays out of the
// capture as the route asked. Nil when no route declares it, so capture pays
// for no lookup.
func sensitiveRoutes(mux routeFinder, sets []plugin.PluginRoutes) func(*http.Request) bool {
	keys := map[string]bool{}
	for _, pr := range sets {
		for _, rd := range pr.Routes {
			if rd.Sensitive {
				keys[grantKey(rd.Method, rd.Pattern)] = true
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return func(r *http.Request) bool {
		pattern := routePatternOf(mux, r)
		if pattern == "" {
			return false
		}
		if keys[grantKey(r.Method, pattern)] {
			return true
		}
		return r.Method == http.MethodHead && keys[grantKey(http.MethodGet, pattern)]
	}
}

// declaredRoutes flattens every set into one list, for a reader that asks
// whether any declaration marks a route.
func declaredRoutes(sets []plugin.PluginRoutes) []plugin.RouteDecl {
	var out []plugin.RouteDecl
	for _, pr := range sets {
		out = append(out, pr.Routes...)
	}
	return out
}

// tenantFeaturesPattern is where the licensing implementation serves the
// features withheld from one tenant.
const tenantFeaturesPattern = "/api/admin/tenant-features/{tenant}"

// licenseRouteServed reports whether the licensing implementation mounted a
// route for method and pattern.
func (o *routerOptions) licenseRouteServed(method, pattern string) bool {
	for _, set := range o.ownerRoutes {
		if !o.licenseOwners[set.Name] {
			continue
		}
		for _, rd := range set.Routes {
			if strings.EqualFold(rd.Method, method) && rd.Pattern == pattern {
				return true
			}
		}
	}
	return false
}

// documentedOwnerRoutes is every owner's routes but the licensing
// implementation's, which the document describes in the engine's own words.
func (o *routerOptions) documentedOwnerRoutes() []plugin.PluginRoutes {
	if len(o.licenseOwners) == 0 {
		return o.ownerRoutes
	}
	out := make([]plugin.PluginRoutes, 0, len(o.ownerRoutes))
	for _, set := range o.ownerRoutes {
		if !o.licenseOwners[set.Name] {
			out = append(out, set)
		}
	}
	return out
}

// declaredRouteSets is every set of route declarations the router mounts,
// the plugins' first and then the other owners', for a reader that treats a
// declaration the same whoever made it.
func (o *routerOptions) declaredRouteSets() []plugin.PluginRoutes {
	if len(o.ownerRoutes) == 0 {
		return o.pluginRoutes
	}
	out := make([]plugin.PluginRoutes, 0, len(o.pluginRoutes)+len(o.ownerRoutes))
	out = append(out, o.pluginRoutes...)
	return append(out, o.ownerRoutes...)
}
