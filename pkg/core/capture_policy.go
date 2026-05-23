package core

import "time"

// The capture policy slot.
//
// The request capture middleware hands every request it wraps to the capture
// sink and asks the sink to keep it for DefaultCaptureTTL. A plugin that wants
// a different answer, such as keeping one route longer, sampling another or
// leaving a tenant's health checks out, implements CapturePolicyProvider,
// which one plugin holds. The activator reads it from the running plugin that
// implements it, and the runtime hands it to the capture middleware on both
// routers. With no provider, or a provider that returns nil, every request is
// captured for DefaultCaptureTTL.
//
// The middleware asks after the handler has run, because the tenant a request
// ran as is resolved partway down the chain and is not known before it. The
// call is made on the request goroutine for every request, so it has to answer
// from memory: a policy that reads the database here adds that read to every
// request the engine serves.
//
// Usage in a plugin:
//
//	var _ core.CapturePolicyProvider = (*Plugin)(nil)
//
//	func (p *Plugin) CapturePolicy() core.CapturePolicy { return p.rules }
//
//	func (r *rules) CaptureFor(tenantID, method, path string) (time.Duration, bool) {
//	    if strings.HasPrefix(path, "/api/v1/health") {
//	        return 0, false
//	    }
//	    return 7 * 24 * time.Hour, true
//	}

// DefaultCaptureTTL is how long a capture is kept when no policy says
// otherwise.
const DefaultCaptureTTL = 24 * time.Hour

// CapturePolicy decides whether the request capture middleware stores one
// request, and for how long.
type CapturePolicy interface {
	// CaptureFor reports whether the request is stored and how long it is
	// kept. tenantID is the tenant the request ran as, empty when it ran
	// outside any tenant. path carries no query string. A capture with a zero
	// or negative ttl is kept for DefaultCaptureTTL. It must be safe for
	// concurrent use.
	CaptureFor(tenantID, method, path string) (ttl time.Duration, capture bool)
}

// CapturePolicyProvider is implemented by a plugin that decides what the
// request capture middleware keeps. Returning nil leaves the default: every
// request, kept for DefaultCaptureTTL.
type CapturePolicyProvider interface {
	CapturePolicy() CapturePolicy
}
