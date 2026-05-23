package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
)

// SecurityHeaders adds defensive HTTP response headers for the admin router.
//
// When CSPNonce runs before this middleware, the CSP uses per-request nonces
// with strict-dynamic instead of 'unsafe-inline'. Without a nonce, the CSP
// omits script-src and style-src entirely.
//
//   - X-Content-Type-Options: nosniff, against MIME sniffing
//   - X-Frame-Options: DENY, against clickjacking
//   - Permissions-Policy: restricts browser feature access (camera, mic, etc.)
//   - Cross-Origin-Opener-Policy: same-origin, against cross-origin side-channel attacks
//   - Cross-Origin-Resource-Policy: same-origin, which blocks cross-origin
//     no-cors embedding of admin JSON. Admin-only: the public API serves JWKS
//     and the OAuth2 token endpoint to third-party consumers, which CORP would
//     break.
//   - Referrer-Policy: strict-origin-when-cross-origin, which limits referrer leakage
//   - Content-Security-Policy: nonce-based strict-dynamic for admin, restrictive for API
//   - Strict-Transport-Security: 2 year HSTS with preload (only when secureMode is true)
//   - Cache-Control: no-store, so authenticated responses are never cached
//   - X-Robots-Tag: noindex, nofollow, noarchive (an authenticated surface has nothing to index)
func SecurityHeaders(secureMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Permissions-Policy",
				"camera=(), microphone=(), geolocation=(), interest-cohort=(), payment=(), usb=(), accelerometer=(), gyroscope=(), magnetometer=(), midi=(), sync-xhr=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			// Nothing on this server is for an index: every route past the
			// login is authenticated, and the login itself is not a page. A
			// crawler that reaches it is told so in the response rather than
			// left to guess from a 401.
			h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")

			nonce := CSPNonceFromContext(r.Context())
			if nonce != "" {
				// Per-request nonce from CSPNonce middleware: strict CSP with
				// nonce-based trust + strict-dynamic to propagate trust to
				// dynamically loaded scripts.
				h.Set("Content-Security-Policy",
					"default-src 'self'; "+
						"script-src 'self' 'nonce-"+nonce+"' 'strict-dynamic'; "+
						"style-src 'self' 'nonce-"+nonce+"'; "+
						"img-src 'self' data:; "+
						"connect-src 'self'; "+
						"font-src 'self'; "+
						"object-src 'none'; "+
						"base-uri 'self'; "+
						"form-action 'self'; "+
						"frame-ancestors 'none'")
			} else {
				// No nonce: strict CSP without script-src/style-src.

				h.Set("Content-Security-Policy",
					"default-src 'self'; "+
						"img-src 'self' data:; "+
						"connect-src 'self'; "+
						"font-src 'self'; "+
						"object-src 'none'; "+
						"base-uri 'self'; "+
						"form-action 'self'; "+
						"frame-ancestors 'none'")
			}

			h.Set("Cache-Control", "no-store")
			if secureMode {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// APISecurityHeaders adds defensive response headers for JSON-only API
// endpoints. Stricter CSP than SecurityHeaders: no scripts, styles, images,
// or fonts needed.
//
//   - Same baseline headers as SecurityHeaders (nosniff, DENY, Permissions-Policy, COOP, etc.)
//   - Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'
//   - Strict-Transport-Security: 2 year HSTS with preload (only when secureMode is true)
func APISecurityHeaders(secureMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Permissions-Policy",
				"camera=(), microphone=(), geolocation=(), interest-cohort=(), payment=(), usb=(), accelerometer=(), gyroscope=(), magnetometer=(), midi=(), sync-xhr=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Content-Security-Policy",
				"default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
			h.Set("Cache-Control", "no-store")
			if secureMode {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
			}
			next.ServeHTTP(w, r)
		})
	}
}

type cspNonceKey struct{}

// CSPNonceFromContext retrieves the per-request CSP nonce. Returns "" when
// CSPNonce middleware is not in the chain.
func CSPNonceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(cspNonceKey{}).(string); ok {
		return v
	}
	return ""
}

// CSPNonce generates a cryptographically random 16-byte nonce per request,
// stores it in context, and sets X-CSP-Nonce for upstream proxy HTML
// injection. Must run BEFORE SecurityHeaders so it can read the nonce.
func CSPNonce() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				// crypto/rand.Read can fail (Linux entropy exhaustion, container
				// seccomp blocking getrandom(2)). Log and proceed without the
				// nonce: SecurityHeaders degrades gracefully to a CSP without
				// the nonce directive.
				slog.ErrorContext(r.Context(), "CSPNonce: crypto/rand.Read failed",
					"err", err,
					"request_id", r.Header.Get("X-Request-ID"),
					"method", r.Method,
					"url", r.URL.String(),
				)
				next.ServeHTTP(w, r)
				return
			}
			nonce := base64.StdEncoding.EncodeToString(b)
			w.Header().Set("X-CSP-Nonce", nonce)
			ctx := context.WithValue(r.Context(), cspNonceKey{}, nonce)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
