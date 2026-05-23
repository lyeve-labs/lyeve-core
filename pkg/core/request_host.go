package core

import (
	"context"
	"net/http"
)

type requestHostCtxKey struct{}

// WithRequestHost records the host the caller addressed when a hop in front
// of the engine vouched for it. The admin console is the case: it calls the
// engine at an internal address, so r.Host names that address and not the
// domain the browser opened. The engine records the console's host only after
// verifying the console's signature over it.
func WithRequestHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, requestHostCtxKey{}, host)
}

// RequestHost returns the host the caller addressed: the one a signed hop
// vouched for, or else the request's own Host. A plugin that resolves a
// tenant from the hostname reads it here rather than from r.Host, so a page
// served through the console resolves the same tenant as a request sent
// straight to the engine under that name.
func RequestHost(r *http.Request) string {
	if h, _ := r.Context().Value(requestHostCtxKey{}).(string); h != "" {
		return h
	}
	return r.Host
}
