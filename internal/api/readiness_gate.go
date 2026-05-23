package api

import (
	"log/slog"
	"net/http"
	"time"
)

// ReadinessGate returns middleware that blocks non-health-check requests
// until the startup probe passes at least once. reg must be the same
// ProbeRegistry used for health probes (so StartTime() reflects the
// startup check).
//
// When reg is nil, the middleware is a no-op (all traffic passes through).
func ReadinessGate(reg *ProbeRegistry) func(http.Handler) http.Handler {
	if reg == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Always pass through health-check endpoints.
			switch r.URL.Path {
			case "/healthz", "/readyz", "/startup":
				next.ServeHTTP(w, r)
				return
			}

			// Once startup has passed at least once, the gate is open.
			if !reg.StartTime().IsZero() {
				next.ServeHTTP(w, r)
				return
			}

			// Still starting: return 503 with Retry-After.
			//
			// Logged here rather than left to the router's access log: this
			// gate wraps the router from outside, so a request refused during
			// startup never reaches the logging middleware and would otherwise
			// be answered with no record that it arrived at all.
			slog.InfoContext(r.Context(), "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", http.StatusServiceUnavailable,
				"reason", "readiness gate closed",
			)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte( // err suppressed: response write to client
				`{"status":"starting","message":"service initializing - retry in a moment"}` + "\n",
			))
		})
	}
}

// ReadinessGateWithTimeout returns a ReadinessGate that additionally
// times out after the given duration. If the startup probe hasn't passed
// within the timeout, the gate opens anyway and logs a warning. This
// prevents a stuck plugin from permanently blocking traffic.
//
// Use when you have a hard SLO for startup time (e.g., 3 seconds).
func ReadinessGateWithTimeout(reg *ProbeRegistry, timeout time.Duration) func(http.Handler) http.Handler {
	gate := ReadinessGate(reg)
	deadline := time.Now().Add(timeout)

	return func(next http.Handler) http.Handler {
		wrapped := gate(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !reg.StartTime().IsZero() {
				wrapped.ServeHTTP(w, r)
				return
			}
			if time.Now().After(deadline) {
				// Gate timeout: let traffic through to avoid
				// a hard-dead pod situation.
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}
