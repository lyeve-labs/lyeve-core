package runtime

import (
	"net/http"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// engineRateLimits lists the limits the engine enforces: the global
// per-address cap when RATE_LIMIT_RPS is set, and the public endpoint table
// in the three layers the public limiter merges. The engine's own rows come
// first, then the limits the mounted routes declare for themselves, then the
// operator's overrides.
//
// A declared limit ships with the route as an engine row ships with the
// engine, so both are reported as the default, and PUBLIC_RATE_LIMITS moves
// either.
//
// A malformed override yields the defaults here. Boot refuses that setting
// before any request is served, so the list is never shown for it.
func engineRateLimits(rps float64, burst int, perTenant bool, publicRoutes []string, publicGlobal string, declared map[string]apimw.PublicRateLimitConfig) []core.EngineRateLimit {
	var out []core.EngineRateLimit
	if rps > 0 {
		out = append(out, core.EngineRateLimit{
			Scope:     "global",
			Endpoint:  "*",
			Rate:      rps,
			Burst:     burst,
			PerTenant: perTenant,
			Source:    "environment",
			Setting:   "RATE_LIMIT_RPS",
		})
	}

	defaults, defaultGlobal := apimw.DefaultPublicRateLimits()
	overrides, globalOverride, err := apimw.ParsePublicRateLimits(publicRoutes, publicGlobal)
	if err != nil {
		overrides, globalOverride = nil, nil
	}
	merged := apimw.MergePublicRateLimits(apimw.MergePublicRateLimits(defaults, declared), overrides)

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := merged[k]
		source := "default"
		if _, ok := overrides[k]; ok {
			source = "environment"
		}
		out = append(out, core.EngineRateLimit{
			Scope:    "public",
			Endpoint: strings.Replace(k, ":", " ", 1),
			Rate:     c.Rate,
			Burst:    c.Burst,
			Source:   source,
			Setting:  "PUBLIC_RATE_LIMITS",
		})
	}

	global, source := defaultGlobal, "default"
	if globalOverride != nil {
		global, source = globalOverride, "environment"
	}
	if global != nil {
		out = append(out, core.EngineRateLimit{
			Scope:    "public",
			Endpoint: "*",
			Rate:     global.Rate,
			Burst:    global.Burst,
			Source:   source,
			Setting:  "PUBLIC_RATE_LIMIT_GLOBAL",
		})
	}
	return out
}

// publishEngineRateLimits hands the host the limits the engine enforces for
// the routes one build of the routers mounts, for a plugin reading
// core.EngineRateLimitsProvider. The runtime calls it for every build, so the
// list follows the plugins that run.
func publishEngineRateLimits(host core.Host, cfg *config.Config, routes []plugin.PluginRoutes) {
	setter, ok := host.(interface {
		WithEngineRateLimits([]core.EngineRateLimit)
	})
	if !ok {
		return
	}
	limits := engineRateLimits(cfg.RateLimitRPS, cfg.RateLimitBurst,
		cfg.RateLimitPerTenant, cfg.PublicRateLimits, cfg.PublicRateLimitGlobal, api.DeclaredPublicRateLimits(routes))
	setter.WithEngineRateLimits(append(limits, generalRouteLimits(routes, limits)...))
}

// generalRouteLimits lists the public routes with no limit of their own, which
// run on the general limit, each with that limit's values. These are the
// routes an administrator may give a limit of their own through
// core.RegisterPublicRouteLimitResolver. With no general limit there is none
// to run on, and the list is empty.
func generalRouteLimits(routes []plugin.PluginRoutes, limits []core.EngineRateLimit) []core.EngineRateLimit {
	own := map[string]bool{}
	var general *core.EngineRateLimit
	for i, l := range limits {
		if l.Scope != "public" {
			continue
		}
		if l.Endpoint == "*" {
			general = &limits[i]
			continue
		}
		own[l.Endpoint] = true
	}
	if general == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []core.EngineRateLimit
	for _, pr := range routes {
		for _, rd := range pr.Routes {
			endpoint := rd.Method + " " + rd.Pattern
			// HEAD follows its GET route's limit, and a credential route
			// never takes one from an administrator.
			if rd.Group != plugin.GroupPublic || own[endpoint] || seen[endpoint] ||
				rd.Method == http.MethodHead || apimw.CredentialRoute(rd.Pattern) {
				continue
			}
			seen[endpoint] = true
			out = append(out, core.EngineRateLimit{
				Scope:    "general",
				Endpoint: endpoint,
				Rate:     general.Rate,
				Burst:    general.Burst,
				Source:   general.Source,
				Setting:  "PUBLIC_RATE_LIMIT_GLOBAL",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}
