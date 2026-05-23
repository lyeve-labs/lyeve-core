package security

// CORSOriginProvider decides whether a custom-domain origin may be reflected
// for the host a request is addressed to.
//
// The question is deliberately asked per (origin, host) rather than as "list
// every origin you know about", because a flat list carries no tenant to scope
// by. An origin registered for one tenant must not be reflected on a request
// to any other.
//
// Scoping by the *session* tenant is not available here. corsMiddleware runs
// ahead of tenant resolution, and a preflight carries no Authorization header
// by specification, so there are no claims to resolve a tenant from and the
// response has to be produced before the browser will send the credentialed
// request at all. The host is the one tenant-bearing value present on a
// preflight, because it is the custom domain the caller dialed.
//
// Implementations must be safe for concurrent use and should not block: this
// is consulted on the request path, including preflights.
type CORSOriginProvider interface {
	// OriginAllowedForHost reports whether origin is a verified custom domain
	// belonging to the same tenant as host. Both are compared as the browser
	// sends them: origin is a full scheme://host[:port], host is the request's
	// Host header. Return false when either is unknown.
	OriginAllowedForHost(origin, host string) bool
}
