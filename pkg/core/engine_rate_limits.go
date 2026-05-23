package core

// EngineRateLimit is one limit the engine enforces from its own settings,
// ahead of every plugin. The engine reads these once at boot, so they change
// only with the setting and a restart. A page that shows them says so rather
// than offering an edit that would not reach the running process.
type EngineRateLimit struct {
	// Scope is "global" for the per-address cap on every request, or
	// "public" for the table covering unauthenticated and auth routes.
	Scope string `json:"scope"`
	// Endpoint is "METHOD /path", or "*" for a cap over every route.
	Endpoint string `json:"endpoint"`
	// Rate is sustained requests per second per address.
	Rate float64 `json:"rate"`
	// Burst is the bucket size per address.
	Burst int `json:"burst"`
	// PerTenant is true when the global cap is kept per tenant and address.
	PerTenant bool `json:"per_tenant,omitempty"`
	// Source is "default" for the shipped value or "environment" when an
	// operator's setting supplied it.
	Source string `json:"source"`
	// Setting names the environment variable that changes the value.
	Setting string `json:"setting"`
}

// EngineRateLimitsProvider is implemented by the engine host and forwarded
// by ScopedHost. A plugin that shows rate limits reads it to list the
// engine's own limits beside its rules, so an operator can see every limiter
// a request passes through.
type EngineRateLimitsProvider interface {
	EngineRateLimits() []EngineRateLimit
}
