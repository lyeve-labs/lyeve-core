package runtime

import (
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
	setter.WithEngineRateLimits(engineRateLimits(cfg.RateLimitRPS, cfg.RateLimitBurst,
		cfg.RateLimitPerTenant, cfg.PublicRateLimits, cfg.PublicRateLimitGlobal, api.DeclaredPublicRateLimits(routes)))
}
