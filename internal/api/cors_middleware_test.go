package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CORS Middleware: Adversarial Penetration Test Suite

// echoHandler returns request info for test assertions.
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
}

// makeCORSMiddleware is a test helper that creates a configured corsMiddleware.
func makeCORSMiddleware(origins []string, allowCredentials bool) func(http.Handler) http.Handler {
	return corsMiddleware(CORSConfig{
		Origins:          origins,
		AllowCredentials: allowCredentials,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID",
	})
}

// makeCORSMiddlewareWithConfig creates a corsMiddleware with full CORSConfig.
func makeCORSMiddlewareWithConfig(cfg CORSConfig) func(http.Handler) http.Handler {
	return corsMiddleware(cfg)
}

// A preflight from an unmatched origin is refused, not passed through.

func TestCORS_PreflightRefusesUnmatchedOrigin(t *testing.T) {
	// A preflight from an unmatched origin gets 204 and no CORS headers.
	origins := []string{"https://trusted.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("OPTIONS from trusted origin returns allow-origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://trusted.example.com")
		req.Header.Set("Access-Control-Request-Method", "DELETE")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code,
			"trusted origin preflight should return 204")
		assert.Equal(t, "https://trusted.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"trusted origin should be echoed back")
	})

	t.Run("OPTIONS from unmatched origin returns 204 with no CORS headers",
		func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
			req.Header.Set("Origin", "https://evil.example.com")
			req.Header.Set("Access-Control-Request-Method", "DELETE")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// Unmatched origin OPTIONS: still 204 but ZERO CORS headers.
			// Browser enforces same-origin. We don't leak capability info.
			assert.Equal(t, http.StatusNoContent, rec.Code,
				"unmatched origin preflight should return 204 (safe: no CORS headers)")
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
				"unmatched origin must NOT get Allow-Origin")
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Methods"),
				"unmatched origin must NOT get Allow-Methods (capability leak)")
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Headers"),
				"unmatched origin must NOT get Allow-Headers (capability leak)")
		})

	t.Run("Non-OPTIONS from unmatched origin passes through to handler", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// Non-OPTIONS requests with unmatched origins pass through without
		// Access-Control-Allow-Origin, so the browser blocks the response.
		assert.Equal(t, http.StatusOK, rec.Code,
			"non-preflight requests pass through (browser enforces)")
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"unmatched origin should NOT have allow-origin header")
	})
}

// Every CORS response carries Vary: Origin so caches keep origins apart.

func TestCORS_VaryOrigin(t *testing.T) {
	origins := []string{"https://tenant-a.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("Vary: Origin is set when origin matches (echo-back)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-a.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://tenant-a.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"origin should be echoed back")

		vary := rec.Header().Get("Vary")
		assert.Contains(t, vary, "Origin",
			"Vary: Origin is required when Access-Control-Allow-Origin is not '*' "+
				"(Fetch Standard §3.2.3); caching proxies otherwise serve wrong origin to clients")
	})

	t.Run("Vary: Origin is set for unmatched origins too (response depends on Origin)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-b.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		vary := rec.Header().Get("Vary")
		assert.Contains(t, vary, "Origin",
			"Vary: Origin must also be set for unmatched origins "+
				"since the response (CORS headers present or absent) depends on Origin")
	})

	t.Run("Preflight response includes Vary: Origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-a.example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Contains(t, rec.Header().Get("Vary"), "Origin",
			"preflight responses must also include Vary: Origin")
	})

	t.Run("Existing non-CORS Vary headers are preserved alongside Origin", func(t *testing.T) {
		// Simulate a downstream middleware that sets Accept-Encoding in Vary.
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Vary", "Accept-Encoding")
			w.WriteHeader(http.StatusOK)
		})
		mw := makeCORSMiddleware(origins, false)(customHandler)

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-a.example.com")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		varyValues := rec.Result().Header["Vary"]
		assert.Contains(t, varyValues, "Origin",
			"Vary must contain Origin even when other Vary values exist")
		assert.Contains(t, varyValues, "Accept-Encoding",
			"existing Vary values from downstream middleware must be preserved")
	})
}

// CORS_ORIGINS applies to every tenant. An origin that belongs to one tenant
// comes from a CORSOriginProvider instead, which the dynamic origin tests
// below cover.

func TestCORS_StaticOriginsAreInstallWide(t *testing.T) {
	t.Run("an origin outside the static list is refused whatever the tenant", func(t *testing.T) {
		origins := []string{"https://global.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-specific.example.com")
		req.Header.Set("X-Tenant-ID", "tenant_abc") // ignored by CORS
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"an origin outside CORS_ORIGINS is refused, whatever tenant the request names")
	})

	t.Run("a static origin is allowed whatever the tenant", func(t *testing.T) {
		origins := []string{"https://tenant-a.example.com", "https://tenant-b.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://tenant-a.example.com")
		req.Header.Set("X-Tenant-ID", "tenant_b")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://tenant-a.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"CORS_ORIGINS is install-wide, so a listed origin passes whatever tenant the request names")
	})
}

// Wildcard origins never combine with credentials.

func TestCORS_WildcardSemantics(t *testing.T) {
	t.Run("Wildcard '*' with credentials=false sets Allow-Origin: *", func(t *testing.T) {
		origins := []string{"*"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://any-site.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"),
			"'*' in origins with credentials=false must set Allow-Origin: *")
		// Vary: Origin must NOT be set when using wildcard (Fetch Standard §3.2.3).
		assert.NotContains(t, rec.Header().Get("Vary"), "Origin",
			"Vary: Origin must NOT be set when using wildcard Allow-Origin")
	})

	t.Run("Wildcard '*' with credentials=true logs warning and ignores wildcard", func(t *testing.T) {
		origins := []string{"*"}
		h := makeCORSMiddleware(origins, true)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://any-site.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// With credentials=true, wildcard is ignored per Fetch Standard.
		// No Allow-Origin should be set since no concrete origin matched.
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"'*' with credentials=true must not set Allow-Origin: * (violates Fetch Standard)")
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"),
			"credentials must not be set when wildcard is ignored")
	})

	t.Run("Exact-match still works for literal origins", func(t *testing.T) {
		origins := []string{"https://exact.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://exact.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://exact.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"exact match should work correctly")
	})
}

// Preflight responses advertise a max-age.

func TestCORS_PreflightMaxAge(t *testing.T) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("Preflight response includes Access-Control-Max-Age", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, "3600", rec.Header().Get("Access-Control-Max-Age"),
			"Access-Control-Max-Age must be set to enable preflight caching; "+
				"without it Chrome defaults to 5 seconds and every cross-origin "+
				"API call triggers a full preflight round-trip")
	})

	t.Run("Custom Max-Age via CORSConfig is respected", func(t *testing.T) {
		cfg := CORSConfig{
			Origins:          []string{"https://app.example.com"},
			AllowCredentials: false,
			PreflightMaxAge:  7200,
			AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
			AllowHeaders:     "Content-Type, Authorization",
		}
		h := makeCORSMiddlewareWithConfig(cfg)(echoHandler())

		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "7200", rec.Header().Get("Access-Control-Max-Age"),
			"custom CORS_MAX_AGE should be respected")
	})
}

// With no expose list configured, no Access-Control-Expose-Headers is sent,
// whatever headers the handler sets. The configured list is covered in
// cors_expose_headers_test.go.

func TestCORS_ExposeHeaders(t *testing.T) {
	t.Run("no expose list configured omits the header", func(t *testing.T) {
		origins := []string{"https://app.example.com"}
		// Use a handler that sets custom headers like the real system does.
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-ID", "req-12345")
			w.Header().Set("X-RateLimit-Remaining", "99")
			w.WriteHeader(http.StatusOK)
		})
		mw := makeCORSMiddleware(origins, false)(customHandler)

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"),
			"with no expose list configured the header is omitted")
	})
}

// CORS_ORIGINS entries match exactly. A glob such as https://*.example.com is
// compared as a literal string, so it never matches a subdomain.

func TestCORS_GlobPatternMatching(t *testing.T) {
	origins := []string{"https://*.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("glob pattern *.example.com does not match a subdomain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"a glob pattern is compared literally, so only an exact origin matches")
	})

	t.Run("Exact match with subdomain literal works", func(t *testing.T) {
		origins := []string{"https://app.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://app.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"exact subdomain match should work")
	})
}

// Allowed methods and headers come from configuration.

func TestCORS_AllowMethods(t *testing.T) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("PATCH method present in Allow-Methods", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts/123", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "PATCH")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		methods := rec.Header().Get("Access-Control-Allow-Methods")
		assert.Contains(t, methods, "PATCH",
			"PATCH must be in Allow-Methods because content updates use it")
		t.Logf("Allow-Methods: %s", methods)
	})

	t.Run("Custom headers X-API-Key and X-Tenant-ID present in Allow-Headers",
		func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
			req.Header.Set("Origin", "https://app.example.com")
			req.Header.Set("Access-Control-Request-Method", "GET")
			req.Header.Set("Access-Control-Request-Headers", "x-api-key, x-tenant-id")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			headers := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
			assert.Contains(t, headers, "x-api-key",
				"X-API-Key must be in allow-headers for API key auth via CORS")
			assert.Contains(t, headers, "x-tenant-id",
				"X-Tenant-ID must be in allow-headers for multi-tenant CORS requests")
		})

	t.Run("Custom headers configurable via CORSConfig", func(t *testing.T) {
		cfg := CORSConfig{
			Origins:          []string{"https://app.example.com"},
			AllowCredentials: false,
			PreflightMaxAge:  3600,
			AllowMethods:     "GET, POST",
			AllowHeaders:     "X-Custom-Header",
		}
		h := makeCORSMiddlewareWithConfig(cfg)(echoHandler())

		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "x-custom-header")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		methods := rec.Header().Get("Access-Control-Allow-Methods")
		assert.Equal(t, "GET, POST", methods,
			"custom Allow-Methods should be respected")
		headers := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
		assert.Contains(t, headers, "x-custom-header",
			"custom Allow-Headers should be respected")
	})

	t.Run("Standard headers Content-Type and Authorization are allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		headers := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
		assert.Contains(t, headers, "content-type",
			"Content-Type should be in allow-headers")
		assert.Contains(t, headers, "authorization",
			"Authorization should be in allow-headers")
	})
}

// Credentialed requests behavior

func TestCORS_CredentialedRequests(t *testing.T) {
	origins := []string{"https://admin.example.com"}

	t.Run("Access-Control-Allow-Credentials set when enabled", func(t *testing.T) {
		h := makeCORSMiddleware(origins, true)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
		req.Header.Set("Origin", "https://admin.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "true",
			rec.Header().Get("Access-Control-Allow-Credentials"),
			"credentials header should be set for admin router")
	})

	t.Run("Access-Control-Allow-Credentials NOT set when disabled", func(t *testing.T) {
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://admin.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"),
			"credentials header should NOT be set for API router")
	})

	t.Run("unmatched origin gets no credentials header", func(t *testing.T) {
		h := makeCORSMiddleware(origins, true)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"),
			"allow-credentials NOT set for unmatched origin (secure behavior)")
	})
}

// Same-origin request handling

func TestCORS_SameOriginRequests(t *testing.T) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("Same-origin request (no Origin header) passes through", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		// No Origin header = same-origin request
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"same-origin requests should pass through")
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"a request with no Origin header gets no CORS headers")
	})
}

// Multiple origins

func TestCORS_MultipleOrigins(t *testing.T) {
	origins := []string{
		"https://app.example.com",
		"https://admin.example.com",
		"https://staging.example.com",
	}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	for _, origin := range origins {
		t.Run("origin="+origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, origin,
				rec.Header().Get("Access-Control-Allow-Origin"),
				"each configured origin should be matched")
		})
	}

	t.Run("unconfigured origin rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"unconfigured origin should not get Allow-Origin header")
	})
}

// Edge cases

func TestCORS_EdgeCases(t *testing.T) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	t.Run("null origin (sandboxed iframe / file://)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "null")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// "null" is a special origin sent by sandboxed iframes and file://.
		// It is compared as the literal string null, so it does not match.
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"'null' origin from sandboxed iframes not matched")
	})

	t.Run("empty origin header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"empty origin header treated as same-origin")
	})

	t.Run("OPTIONS without Origin header (non-CORS preflight)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/health", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// The middleware answers every OPTIONS request with 204, with or
		// without an Origin header, so none reaches the handler.
		assert.Equal(t, http.StatusNoContent, rec.Code,
			"an OPTIONS request with no Origin header is answered with 204 before the handler")
	})

	t.Run("trailing slash in origin", func(t *testing.T) {
		// Should NOT match: trailing slash changes the origin string.
		req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com/")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"an origin with a trailing slash is a different string, so it does not match")
	})

	t.Run("case sensitivity in origin", func(t *testing.T) {
		// Origin header values are compared case-sensitively, as the spec
		// requires.
		t.Run("lowercase matches", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
			req.Header.Set("Origin", "https://app.example.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, "https://app.example.com",
				rec.Header().Get("Access-Control-Allow-Origin"))
		})

		t.Run("uppercase does not match (spec-compliant)", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
			req.Header.Set("Origin", "HTTPS://APP.EXAMPLE.COM")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
				"uppercase origin should not match, because origins compare case-sensitively")
		})
	})
}

// Router integration: verify corsMiddleware configuration in both routers

func TestCORS_RouterIntegration(t *testing.T) {
	t.Run("Admin router uses credentials=true", func(t *testing.T) {
		// Verify by testing that the router-level setting propagates.
		// We test the middleware directly since we can't easily mock a full router.
		origins := []string{"http://localhost:5173"}
		h := makeCORSMiddleware(origins, true)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "true",
			rec.Header().Get("Access-Control-Allow-Credentials"),
			"admin router must allow credentials for HTTP-only cookie auth")
	})

	t.Run("API router uses credentials=false", func(t *testing.T) {
		origins := []string{"https://api-consumer.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://api-consumer.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"),
			"API router should not set allow-credentials (Bearer token auth)")
	})
}

// Property-based / fuzzing: random origin strings

func TestCORS_Fuzzing_RandomOrigins(t *testing.T) {
	// This is a deterministic fuzz test: not true fuzzing, but probes
	// a representative set of edge-case origin strings.
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	attackOrigins := []string{
		"https://app.example.com.evil.com",    // suffix attack
		"https://app.example.com@evil.com",    // userinfo confusion
		"https://app.example.com:evil.com",    // bad port format
		"https://app.example.com%40evil.com",  // URL encoding
		"https://app.example.com\\@evil.com",  // backslash
		"https://app.example.com%2f.evil.com", // encoded slash
		"https://app.example.com#evil.com",    // fragment
		"https://app.example.com?origin=evil", // query string
		"https://app.example.com.",            // trailing dot
		"https://app.example.com:443",         // explicit default port
		"HTTPS://APP.EXAMPLE.COM",             // uppercase
		" https://app.example.com",            // leading space
		"https://app.example.com ",            // trailing space
		"https://app.example.com\t",           // tab
		"https://app.example.com\n.evil.com",  // newline injection
	}

	for _, attackOrigin := range attackOrigins {
		t.Run("origin_fuzz="+truncateForName(attackOrigin), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/content/posts", nil)
			req.Header.Set("Origin", attackOrigin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// All attack origins should NOT match.
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
				"fuzzed origin %q should not match trusted origin", attackOrigin)
		})
	}
}

func truncateForName(s string) string {
	// Truncate for readable test names.
	if len(s) > 30 {
		return s[:27] + "..."
	}
	return strings.ReplaceAll(s, "\t", "\\t")
}

// Benchmark: CORS middleware overhead

func BenchmarkCORSMiddleware_MatchedOrigin(b *testing.B) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req.Header.Set("Origin", "https://app.example.com")

	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkCORSMiddleware_UnmatchedOrigin(b *testing.B) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req.Header.Set("Origin", "https://evil.example.com")

	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkCORSMiddleware_NoOrigin(b *testing.B) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)

	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkCORSMiddleware_Preflight(b *testing.B) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")

	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkCORSMiddleware_MultipleOrigins(b *testing.B) {
	origins := []string{
		"https://app1.example.com",
		"https://app2.example.com",
		"https://app3.example.com",
		"https://app4.example.com",
		"https://app5.example.com",
		"https://app6.example.com",
		"https://app7.example.com",
		"https://app8.example.com",
		"https://app9.example.com",
		"https://app10.example.com",
	}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	// Match the last origin (worst case for linear scan).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req.Header.Set("Origin", "https://app10.example.com")

	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

// Preflight round-trip overhead, the cost Access-Control-Max-Age saves on
// repeat requests.

func BenchmarkPreflightFullRoundtrip(b *testing.B) {
	origins := []string{"https://app.example.com"}
	h := makeCORSMiddleware(origins, false)(echoHandler())

	// Simulate a browser-like flow: OPTIONS -> GET.
	// Without Access-Control-Max-Age, this happens on EVERY cross-origin request.
	b.ResetTimer()
	for b.Loop() {
		// Preflight
		preReq := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		preReq.Header.Set("Origin", "https://app.example.com")
		preReq.Header.Set("Access-Control-Request-Method", "GET")
		preRec := httptest.NewRecorder()
		h.ServeHTTP(preRec, preReq)

		// Actual request
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

// The middleware wraps the next handler and still runs it.

func TestCORSMiddleware_RunsTheNextHandler(t *testing.T) {
	t.Run("corsMiddleware returns correct function type", func(t *testing.T) {
		mw := corsMiddleware(CORSConfig{
			Origins:          []string{"https://example.com"},
			AllowCredentials: false,
			PreflightMaxAge:  3600,
			AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
			AllowHeaders:     "Content-Type, Authorization",
		})
		require.NotNil(t, mw, "corsMiddleware should return a middleware function")

		h := mw(echoHandler())
		require.NotNil(t, h, "middleware should produce a handler")
	})

	t.Run("response headers set in correct order", func(t *testing.T) {
		origins := []string{"https://order.example.com"}
		h := makeCORSMiddleware(origins, false)(echoHandler())

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Origin", "https://order.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// Verify the handler's X-Test-Path is also set: confirms
		// the middleware chain works correctly.
		assert.Equal(t, "/test",
			rec.Header().Get("X-Test-Path"),
			"handler should execute after CORS middleware")
	})
}

// Dynamic origins from a CORSOriginProvider are filtered by AllowedDomains.

// testOriginProvider implements security.CORSOriginProvider. It models what a
// real provider has to do: an origin is admitted only when the custom domain
// it names and the host being addressed belong to the same tenant.
type testOriginProvider struct {
	tenantOf map[string]string // host -> tenant
}

func (p *testOriginProvider) OriginAllowedForHost(origin, host string) bool {
	originHost := hostFromOrigin(origin)
	if originHost == "" {
		return false
	}
	originTenant, ok := p.tenantOf[originHost]
	if !ok {
		return false
	}
	hostTenant, ok := p.tenantOf[host]
	if !ok {
		return false
	}
	return originTenant == hostTenant
}

func TestFilterAllowedDynamicOrigins(t *testing.T) {
	t.Run("fail-closed: empty allowed domains rejects all", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://tenant-a.custom.com"},
			nil,
		)
		assert.Empty(t, result, "empty allowed domains must reject all dynamic origins")
	})

	t.Run("fail-closed: empty allowed domains (non-nil slice) rejects all", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://tenant-a.custom.com"},
			[]string{},
		)
		assert.Empty(t, result, "empty allowed domains (non-nil slice) must reject all dynamic origins")
	})

	t.Run("suffix match passes through", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://tenant-a.customer.com"},
			[]string{"customer.com"},
		)
		assert.Equal(t, []string{"https://tenant-a.customer.com"}, result)
	})

	t.Run("non-matching origin is rejected", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://tenant-a.evil.com"},
			[]string{"customer.com"},
		)
		assert.Empty(t, result, "non-matching origin must be rejected")
	})

	t.Run("subdomain matches parent domain suffix", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://analytics.tenant-a.customer.com"},
			[]string{"customer.com"},
		)
		assert.Equal(t, []string{"https://analytics.tenant-a.customer.com"}, result)
	})

	t.Run("of multiple origins only the matching ones pass through", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{
				"https://tenant-a.customer.com",
				"https://tenant-b.other.io",
				"https://tenant-c.customer.com",
			},
			[]string{"customer.com"},
		)
		assert.Equal(t, []string{"https://tenant-a.customer.com", "https://tenant-c.customer.com"}, result)
	})

	t.Run("multiple allowed domains", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{
				"https://tenant-a.customer.com",
				"https://tenant-b.other.io",
			},
			[]string{"customer.com", "other.io"},
		)
		assert.Equal(t, 2, len(result))
		assert.Contains(t, result, "https://tenant-a.customer.com")
		assert.Contains(t, result, "https://tenant-b.other.io")
	})

	t.Run("empty origin list from provider returns nil", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			nil,
			[]string{"customer.com"},
		)
		assert.Empty(t, result)
	})

	t.Run("port in origin is preserved", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"https://tenant-a.customer.com:443"},
			[]string{"customer.com"},
		)
		assert.Equal(t, []string{"https://tenant-a.customer.com:443"}, result)
	})

	t.Run("http scheme works", func(t *testing.T) {
		result := filterAllowedDynamicOrigins(
			[]string{"http://tenant-a.customer.com"},
			[]string{"customer.com"},
		)
		assert.Equal(t, []string{"http://tenant-a.customer.com"}, result)
	})
}

func TestHostFromOrigin(t *testing.T) {
	tests := []struct {
		origin   string
		expected string
	}{
		{"https://tenant-a.customer.com", "tenant-a.customer.com"},
		{"https://tenant-a.customer.com:443", "tenant-a.customer.com"},
		{"http://tenant-a.customer.com", "tenant-a.customer.com"},
		{"tenant-a.customer.com", "tenant-a.customer.com"},
		{"", ""},
		{"://malformed", ""},
	}
	for _, tc := range tests {
		t.Run(tc.origin, func(t *testing.T) {
			got := hostFromOrigin(tc.origin)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestCORSDynamicOrigins_FailClosedWithMiddleware(t *testing.T) {
	// Test that when AllowedDomains is empty, dynamic origins are NOT
	// reflected back even when a CORSOriginProvider is set.
	origins := []string{"https://static.example.com"}
	allowedDomains := []string{} // empty = fail-closed

	// Install a dynamic provider that returns an origin not in the static list.
	provider := &testOriginProvider{tenantOf: map[string]string{
		"tenant-a.custom.com": "a",
		"a.lyeve.test":        "a",
	}}
	setDynamicCORSProvider(provider)
	defer setDynamicCORSProvider(nil)

	h := corsMiddleware(CORSConfig{
		Origins:          origins,
		AllowCredentials: false,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization",
		AllowedDomains:   allowedDomains,
	})(echoHandler())

	t.Run("static origin still works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("Origin", "https://static.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://static.example.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"static origin must still be allowed")
	})

	t.Run("dynamic origin blocked by empty AllowedDomains", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://tenant-a.custom.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"dynamic origin must be rejected when AllowedDomains is empty")
	})
}

func TestCORSDynamicOrigins_AllowedDomainsPassThrough(t *testing.T) {
	// Test that dynamic origins matching AllowedDomains are reflected back.
	origins := []string{"https://static.example.com"}
	allowedDomains := []string{"customer.com"}

	provider := &testOriginProvider{tenantOf: map[string]string{
		"tenant-a.customer.com": "a",
		"a.lyeve.test":          "a",
	}}
	setDynamicCORSProvider(provider)
	defer setDynamicCORSProvider(nil)

	h := corsMiddleware(CORSConfig{
		Origins:          origins,
		AllowCredentials: false,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST, PUT, DELETE, PATCH, OPTIONS",
		AllowHeaders:     "Content-Type, Authorization",
		AllowedDomains:   allowedDomains,
	})(echoHandler())

	t.Run("matching dynamic origin passes through", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://tenant-a.customer.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, "https://tenant-a.customer.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"matching dynamic origin must be allowed when domain is in AllowedDomains")
	})

	t.Run("non-matching dynamic origin is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://tenant-a.evil.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"non-matching dynamic origin must be rejected")
	})
}

func TestCORSDynamicOrigins_PreflightAllowedDomains(t *testing.T) {
	// Verify that preflight requests also respect the AllowedDomains filter.
	allowedDomains := []string{"customer.com"}

	provider := &testOriginProvider{tenantOf: map[string]string{
		"tenant-a.customer.com": "a",
		"a.lyeve.test":          "a",
	}}
	setDynamicCORSProvider(provider)
	defer setDynamicCORSProvider(nil)

	h := corsMiddleware(CORSConfig{
		Origins:          nil,
		AllowCredentials: true,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST",
		AllowHeaders:     "Content-Type",
		AllowedDomains:   allowedDomains,
	})(echoHandler())

	t.Run("matching domain preflight succeeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://tenant-a.customer.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, "https://tenant-a.customer.com",
			rec.Header().Get("Access-Control-Allow-Origin"),
			"preflight for matching dynamic domain must succeed")
	})

	t.Run("non-matching domain preflight rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://tenant-a.evil.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"preflight for non-matching dynamic domain must have no allow-origin")
	})
}

// An origin verified for one tenant must not be reflected on a request
// addressed to another. The provider answers for an origin and a host
// together, which gives the middleware a tenant to scope by.
func TestCORSDynamicOrigins_RefusesAnOriginBelongingToAnotherTenant(t *testing.T) {
	provider := &testOriginProvider{tenantOf: map[string]string{
		"a.customer.com": "a",
		"b.customer.com": "b",
		"a.lyeve.test":   "a",
		"b.lyeve.test":   "b",
	}}
	setDynamicCORSProvider(provider)
	defer setDynamicCORSProvider(nil)

	h := corsMiddleware(CORSConfig{
		AllowCredentials: true,
		PreflightMaxAge:  3600,
		AllowMethods:     "GET, POST",
		AllowHeaders:     "Content-Type",
		AllowedDomains:   []string{"customer.com"},
	})(echoHandler())

	t.Run("its own host reflects the origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Host = "a.lyeve.test"
		req.Header.Set("Origin", "https://a.customer.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, "https://a.customer.com", rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("another tenant's host does not", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Host = "b.lyeve.test"
		req.Header.Set("Origin", "https://a.customer.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
			"tenant a's verified domain must not be reflected on tenant b's host")
	})

	t.Run("the same rule holds on a preflight, which carries no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/api/v1/content/posts", nil)
		req.Host = "b.lyeve.test"
		req.Header.Set("Origin", "https://a.customer.com")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}
