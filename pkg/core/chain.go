package core

import "net/http"

// The phased middleware chain.
//
// A plugin that contributes request middleware to the routers says where
// each piece goes, as a phase, instead of the engine knowing the plugin by
// name. The engine mounts every running plugin's entries below auth and
// tenancy and above the routes, ordered by phase, then by plugin name, then
// in the order the plugin lists them.
//
// The phases leave room between them, so an entry can sit between two named
// ones. Where an entry sits decides what it sees: a quota gate after the rate
// limiter never counts a request the limiter refused. The engine keeps two
// positions of its own in the same chain. The request sampler mounts after
// every entry below ChainPhaseAnalytics, and response masking after every
// entry below ChainPhaseQuota.

// ChainPhase is where an entry mounts in the chain. Lower phases see the
// request first.
type ChainPhase int

const (
	// ChainPhaseRateLimit is for limiters that refuse a request before anything
	// else spends work on it.
	ChainPhaseRateLimit ChainPhase = 100
	// ChainPhaseMonitor is for middleware that times or records the request.
	ChainPhaseMonitor ChainPhase = 200
	// ChainPhaseAnalytics is for middleware that counts requests the limiters
	// let through.
	ChainPhaseAnalytics ChainPhase = 300
	// ChainPhaseFirewall is for request inspection.
	ChainPhaseFirewall ChainPhase = 400
	// ChainPhaseQuota is for tenant quotas and per-key metering, which count
	// only what inspection let through.
	ChainPhaseQuota ChainPhase = 500
	// ChainPhaseResidency is for write guards that decide where a tenant's
	// data may be written. Every lower phase has let the request through by
	// then, so a guard here looks up a region only for a write the chain
	// before it accepted.
	ChainPhaseResidency ChainPhase = 600
)

// ChainRouter says which routers an entry mounts on.
type ChainRouter uint8

const (
	// ChainAdmin is the admin router.
	ChainAdmin ChainRouter = 1 << iota
	// ChainAPI is the content API router.
	ChainAPI
	// ChainBoth is both routers.
	ChainBoth = ChainAdmin | ChainAPI
)

// ChainEntry is one middleware and where it mounts.
type ChainEntry struct {
	Phase      ChainPhase
	Routers    ChainRouter
	Middleware func(http.Handler) http.Handler
}

// ChainMiddlewareProvider is implemented by a plugin that contributes
// middleware to the routers' chain. The engine reads it from every running
// plugin. An entry with no middleware, or with no router, mounts nothing.
type ChainMiddlewareProvider interface {
	ChainMiddleware() []ChainEntry
}
