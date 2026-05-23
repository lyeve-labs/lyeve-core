package middleware

import (
	"fmt"
	"strconv"
	"strings"
)

// Public rate limits are operational configuration, not constants.
//
// The shipped table suits one origin serving browsers directly. It does not
// suit an origin behind a CDN that collapses a region onto a handful of egress
// addresses, an API whose clients are servers rather than people, a deployment
// behind a trusted reverse proxy that terminates on loopback, or a load test
// driven from a single machine. Every one of those counts many callers as one
// address, so the per-IP caps throttle a caller against itself.
//
// The settings are read by config.Load and handed to the routers, in the same
// shape as every other limit the engine exposes:
//
//	PUBLIC_RATE_LIMITS=POST:/api/v1/auth/token=20:100,POST:/api/admin/auth/login=10:50
//	PUBLIC_RATE_LIMIT_GLOBAL=200:400
//
// Each entry is METHOD:/pattern=rate:burst, where pattern is the route exactly
// as registered, braces and all. Overrides merge over the defaults rather than
// replacing them: raising one endpoint must not silently drop the protection on
// every other, which is the mistake this shape exists to prevent.

// ParsePublicRateLimits turns the two operator settings into overrides for
// DefaultPublicRateLimits. perRoute carries METHOD:/pattern=rate:burst entries,
// global a single rate:burst for the aggregate per-IP cap. Both may be empty,
// which yields no overrides and leaves the shipped defaults exactly as they are.
//
// A malformed entry is an error rather than a warning. A dropped entry would
// leave an operator believing a cap had moved when it had not, and the
// difference only shows up as traffic being refused in production.
func ParsePublicRateLimits(perRoute []string, global string) (map[string]PublicRateLimitConfig, *PublicRateLimitConfig, error) {
	var (
		out      map[string]PublicRateLimitConfig
		rejected []string
	)
	for _, entry := range perRoute {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, spec, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			rejected = append(rejected, fmt.Sprintf("%q (expected METHOD:/pattern=rate:burst)", entry))
			continue
		}
		cfg, ok := parseRateBurst(spec)
		if !ok {
			rejected = append(rejected, fmt.Sprintf("%q (rate must be a positive number and burst a positive integer)", entry))
			continue
		}
		if out == nil {
			out = make(map[string]PublicRateLimitConfig, len(perRoute))
		}
		out[key] = cfg
	}

	var globalCfg *PublicRateLimitConfig
	if g := strings.TrimSpace(global); g != "" {
		cfg, ok := parseRateBurst(g)
		if !ok {
			rejected = append(rejected, fmt.Sprintf("global %q (expected rate:burst)", g))
		} else {
			globalCfg = &cfg
		}
	}

	if len(rejected) > 0 {
		return nil, nil, fmt.Errorf("invalid public rate limit: %s", strings.Join(rejected, "; "))
	}
	return out, globalCfg, nil
}

// MergePublicRateLimits applies overrides on top of base and returns the
// result. Neither input is modified, so a caller keeping the default table
// around still has the defaults.
func MergePublicRateLimits(base, overrides map[string]PublicRateLimitConfig) map[string]PublicRateLimitConfig {
	merged := make(map[string]PublicRateLimitConfig, len(base)+len(overrides))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}

// parseRateBurst reads "rate:burst". Both must be positive: a zero rate would
// read as "no limit" to anyone skimming the setting while actually refusing
// every request, and a zero burst refuses the first one.
func parseRateBurst(spec string) (PublicRateLimitConfig, bool) {
	rawRate, rawBurst, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok {
		return PublicRateLimitConfig{}, false
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(rawRate), 64)
	if err != nil || rate <= 0 {
		return PublicRateLimitConfig{}, false
	}
	burst, err := strconv.Atoi(strings.TrimSpace(rawBurst))
	if err != nil || burst <= 0 {
		return PublicRateLimitConfig{}, false
	}
	return PublicRateLimitConfig{Rate: rate, Burst: burst}, true
}
