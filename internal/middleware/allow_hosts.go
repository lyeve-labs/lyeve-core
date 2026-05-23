package middleware

import (
	"log/slog"
	"net"
	"net/http"
)

// AllowedHosts returns middleware that validates the Host header against a
// configured allowlist. Requests whose Host does not match (after port
// stripping) receive HTTP 421 Misdirected Request.
//
// When allowed is empty the middleware is a no-op: all Host headers pass
// through. This suits deployments that rely on upstream proxies or load
// balancers to perform Host validation.
//
// Place this middleware BEFORE HTTPSRedirect in the chain. Without it,
// an attacker can poison the redirect target by setting Host to a
// malicious domain:
//
//	curl -H "Host: evil.com" http://legit.example.com/path
//	-> 301 https://evil.com/path
//
// With AllowedHosts active, the request is rejected at 421 before
// HTTPSRedirect can construct a poisoned target.
func AllowedHosts(allowed []string) func(http.Handler) http.Handler {
	if len(allowed) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	set := make(map[string]struct{}, len(allowed))
	for _, h := range allowed {
		set[h] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := stripHostPort(r.Host)
			if _, ok := set[host]; ok {
				next.ServeHTTP(w, r)
				return
			}

			slog.WarnContext(r.Context(),
				"allowed_hosts: rejected request with disallowed Host header",
				"host", r.Host,
				"allowed", allowed,
				"method", r.Method,
				"path", r.URL.Path,
			)
			http.Error(w, `{"error":"misdirected request"}`, http.StatusMisdirectedRequest)
		})
	}
}

// stripHostPort strips the port from host:port. Returns host unchanged when no port is present.
func stripHostPort(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return host // no port
	}
	return h
}
