package middleware

import (
	"bufio"
	"net"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// APIKeyAudit returns middleware that records every API-key-authenticated
// request to the audit log. It wraps the ResponseWriter to capture the HTTP
// status code and extracts the client IP from the request using
// ClientIPTrusted, which only honors X-Forwarded-For when the immediate
// proxy is in trustedCIDRs.
//
// logFn is called with apiKeyID, method, path, status, and client IP after
// the handler completes. It should be fire-and-forget: failures in audit
// logging must not affect the response.
//
// JWT-authenticated requests bypass audit logging.
//
// When trustedCIDRs is empty, XFF is never trusted and RemoteAddr is used
// directly (spoof-proof, proxy-blind). Pass ParseCIDRs(cfg.TrustedProxies)
// from the runtime to enable proxy-aware IP extraction.
func APIKeyAudit(logFn func(apiKeyID, method, path string, status int, ip string), trustedCIDRs []*net.IPNet) func(http.Handler) http.Handler {
	if logFn == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := core.GetClaims(r.Context())
			if claims == nil || !claims.IsAPIKey {
				next.ServeHTTP(w, r)
				return
			}

			ip := reqparse.ClientIPTrusted(r, trustedCIDRs)
			crw := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(crw, r)

			// Fire-and-forget: audit failures do not affect the response.
			logFn(claims.UserID, r.Method, r.URL.Path, crw.status, ip)
		})
	}
}

// statusRecorder captures the HTTP status code written by the handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader captures the HTTP status code before delegating.
func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush propagates to the underlying writer so streaming responses (SSE) that
// pass through the audit wrapper are not buffered.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes a connection upgrade through to the writer underneath.
// Without it a WebSocket or SSE handler mounted below this middleware cannot
// take the socket, and the handshake fails with the response already committed.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
