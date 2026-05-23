package middleware

import (
	"net"
	"net/http"
)

// StripUntrustedProxyHeaders strips proxy-set headers when the peer is not
// in trustedCIDRs. Stripped headers: True-Client-IP, X-Forwarded-For,
// X-Real-IP, X-TLS-Fingerprint. Without this, attackers spoof these headers
// to defeat per-IP brute-force counters.
//
// X-Forwarded-Proto is stripped from untrusted peers only when trustedCIDRs is
// non-empty. HTTPSRedirect reads it, so stripping it unconditionally would put
// every reverse-proxy deployment that has not declared its proxy CIDRs into a
// redirect loop: the proxy terminates TLS, forwards plaintext, the server sees
// no proto claim and redirects to HTTPS, and round it goes. With no declared
// topology a proxy and a client are indistinguishable, and an outage is the
// worse failure. A spoofed proto only skips the caller's own redirect, so the
// residual exposure is to the caller alone. Declare TRUSTED_PROXIES in
// production and it is closed.
//
// When trustedCIDRs is nil or empty, the remaining proxy headers are always
// stripped (fail-safe). Must run BEFORE ClientAddress so it inspects
// the original r.RemoteAddr.
func StripUntrustedProxyHeaders(trustedCIDRs []*net.IPNet) func(http.Handler) http.Handler {
	unconditional := len(trustedCIDRs) == 0

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !unconditional {
				peer := peerIP(r.RemoteAddr)
				for _, cidr := range trustedCIDRs {
					if parsed := net.ParseIP(peer); parsed != nil && cidr.Contains(parsed) {
						// Peer is trusted: leave proxy headers intact.
						next.ServeHTTP(w, r)
						return
					}
				}
				r.Header.Del("X-Forwarded-Proto")
			}
			// Strip proxy-only headers.
			r.Header.Del("True-Client-IP")
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-IP")
			r.Header.Del("X-TLS-Fingerprint")
			next.ServeHTTP(w, r)
		})
	}
}

// peerIP extracts the IP from a host:port string. Returns the string
// unchanged when splitting fails.
func peerIP(addr string) string {
	if ip, _, err := net.SplitHostPort(addr); err == nil {
		return ip
	}
	return addr
}
