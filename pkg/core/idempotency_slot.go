package core

import "net/http"

// The idempotency slot.
//
// A plugin that enforces Idempotency-Key on the write routes of both routers
// replays the stored response for a key seen before, answers 409 for a key in
// flight, and refuses a key reused on another endpoint or with another body.
// A plugin cannot wrap the routers itself, so the engine keeps one slot for
// it. The plugin implements IdempotencyMiddlewareProvider, which one plugin
// holds. The activator reads it from the running plugin that implements it,
// and the runtime mounts what it returns on the admin and the API chain in
// one fixed position: after every other chain-level middleware and before the
// routes.
// That position is the capability. Below it, tenancy is resolved, so the key
// the plugin stores is scoped by TenantIDFromCtx and two tenants spending the
// same key never see each other's response. Above the routes, the middleware
// sees the request before any handler runs, so a replay costs no handler work
// and an in-flight key blocks the second caller before it can write.
//
// Usage in the plugin:
//
//	var _ core.IdempotencyMiddlewareProvider = (*Plugin)(nil)
//
//	func (p *Plugin) IdempotencyMiddleware() func(http.Handler) http.Handler {
//	    return p.middleware.Handler
//	}

// IdempotencyMiddlewareProvider is implemented by a plugin that enforces
// Idempotency-Key. Returning nil mounts nothing, and every write route then
// runs without replay protection, which is what an install without such a
// plugin gets.
type IdempotencyMiddlewareProvider interface {
	IdempotencyMiddleware() func(http.Handler) http.Handler
}
