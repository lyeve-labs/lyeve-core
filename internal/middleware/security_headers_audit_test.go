package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// The security headers each router sets: Permissions-Policy, COOP, HSTS
// preload, CSP base-uri and form-action, and a separate CSP for the API.

func TestSecurityHeaders_BaselineHeadersPresent(t *testing.T) {
	t.Run("secure_mode_true", func(t *testing.T) {
		mw := apimw.SecurityHeaders(true)
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		h := rec.Header()

		// X-Content-Type-Options: nosniff
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
		}

		// X-Frame-Options: DENY
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
		}

		// Permissions-Policy: present
		pp := h.Get("Permissions-Policy")
		if pp == "" {
			t.Error("Permissions-Policy header is missing")
		}
		if !strings.Contains(pp, "camera=()") {
			t.Errorf("Permissions-Policy missing camera restriction: %q", pp)
		}

		// Cross-Origin-Opener-Policy: same-origin
		if got := h.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Errorf("Cross-Origin-Opener-Policy = %q, want same-origin", got)
		}

		// Referrer-Policy: strict-origin-when-cross-origin
		if got := h.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
		}
		if got := h.Get("X-Robots-Tag"); got != "noindex, nofollow, noarchive" {
			t.Errorf("X-Robots-Tag = %q, want noindex, nofollow, noarchive", got)
		}

		// Content-Security-Policy: present with base-uri and form-action
		csp := h.Get("Content-Security-Policy")
		if csp == "" {
			t.Error("Content-Security-Policy header is missing")
		}
		if !strings.Contains(csp, "base-uri") {
			t.Error("CSP missing base-uri directive")
		}
		if !strings.Contains(csp, "form-action") {
			t.Error("CSP missing form-action directive")
		}

		// Strict-Transport-Security: present in secure mode with preload
		hsts := h.Get("Strict-Transport-Security")
		if hsts == "" {
			t.Error("Strict-Transport-Security header is missing in secure mode")
		}
		if !strings.Contains(hsts, "preload") {
			t.Errorf("HSTS missing preload directive: %q", hsts)
		}
		if !strings.Contains(hsts, "max-age=63072000") {
			t.Errorf("HSTS max-age should be 63072000 (2yr): %q", hsts)
		}

		// Cache-Control: no-store
		if got := h.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want %q", got, "no-store")
		}
	})

	t.Run("secure_mode_false", func(t *testing.T) {
		mw := apimw.SecurityHeaders(false)
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// HSTS must NOT be present when secureMode=false
		if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS header should be absent when secureMode=false, got %q", got)
		}

		// Other headers still present
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Error("X-Content-Type-Options missing in non-secure mode")
		}

		// Permissions-Policy still present in non-secure mode
		if got := rec.Header().Get("Permissions-Policy"); got == "" {
			t.Error("Permissions-Policy missing in non-secure mode")
		}
	})
}

func TestSecurityHeaders_CSP_OmitsUnsafeInline(t *testing.T) {
	// The CSP never allows 'unsafe-inline'.
	// The admin SecurityHeaders with CSPNonce in chain uses nonce+strict-dynamic.
	// Without CSPNonce (tested directly here), it omits script-src/style-src.

	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")

	// 'unsafe-inline' must be absent.
	dangerousDirectives := []string{
		"'unsafe-inline'",
	}
	for _, dir := range dangerousDirectives {
		if strings.Contains(csp, dir) {
			t.Errorf("CSP contains %q, which must be absent. CSP: %q", dir, csp)
		}
	}

	// Verify required directives are present
	for _, dir := range []string{"base-uri", "form-action", "frame-ancestors"} {
		if !strings.Contains(csp, dir) {
			t.Errorf("CSP missing %s directive", dir)
		}
	}
}

func TestSecurityHeaders_HSTS_IncludesPreload(t *testing.T) {
	// HSTS carries the preload directive and a 2-year max-age.

	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	hsts := rec.Header().Get("Strict-Transport-Security")

	if hsts == "" {
		t.Fatal("HSTS header missing, so preload cannot be verified")
	}

	if !strings.Contains(hsts, "includeSubDomains") {
		t.Error("HSTS missing includeSubDomains directive")
	}

	if !strings.Contains(hsts, "preload") {
		t.Error("HSTS missing 'preload' directive")
	}

	if !strings.Contains(hsts, "max-age=63072000") {
		t.Errorf("HSTS max-age should be 63072000 (2yr) for preload eligibility, got: %q", hsts)
	}
}

func TestSecurityHeaders_PermissionsPolicy_Present(t *testing.T) {
	// Permissions-Policy restricts the listed browser features.

	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	pp := rec.Header().Get("Permissions-Policy")
	if pp == "" {
		t.Fatal("Permissions-Policy header is missing")
	}

	requiredFeatures := []string{
		"camera=()",
		"microphone=()",
		"geolocation=()",
		"payment=()",
	}
	for _, feat := range requiredFeatures {
		if !strings.Contains(pp, feat) {
			t.Errorf("Permissions-Policy missing %s: %q", feat, pp)
		}
	}
}

func TestSecurityHeaders_CrossOriginOpenerPolicy_Present(t *testing.T) {
	// Cross-Origin-Opener-Policy is same-origin.

	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	coop := rec.Header().Get("Cross-Origin-Opener-Policy")
	if coop == "" {
		t.Fatal("Cross-Origin-Opener-Policy header is missing")
	}
	if coop != "same-origin" {
		t.Errorf("Cross-Origin-Opener-Policy = %q, want same-origin", coop)
	}
}

func TestSecurityHeaders_CrossOriginResourcePolicy_AdminOnly(t *testing.T) {
	// The admin router carries CORP so a cross-origin page cannot pull admin
	// JSON in as a no-cors subresource. The public API deliberately does not:
	// JWKS and the OAuth2 token endpoint are consumed cross-origin by design.

	for _, secure := range []bool{true, false} {
		mw := apimw.SecurityHeaders(secure)
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("admin CORP (secureMode=%v) = %q, want same-origin", secure, got)
		}
	}

	apiMW := apimw.APISecurityHeaders(true)
	apiHandler := apiMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	apiReq := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	apiRec := httptest.NewRecorder()
	apiHandler.ServeHTTP(apiRec, apiReq)

	if got := apiRec.Header().Get("Cross-Origin-Resource-Policy"); got != "" {
		t.Errorf("public API CORP = %q, want it absent", got)
	}
}

// TestSecurityHeaders_CrossOriginIsolation covers the isolation headers the
// middleware sets, and pins the one it deliberately does not.
//
// COOP and CORP ship. COEP does not, and that is a choice rather than an
// oversight: require-corp makes the browser refuse every cross-origin
// subresource that does not carry its own CORP or CORS header, so turning it on
// without auditing fonts, images and embeds first breaks the admin rather than
// hardening it. Only COOP and COEP together make a context crossOriginIsolated,
// so cross-origin isolation is partial until that audit happens.
//
// The absence is asserted so that adding COEP fails here and forces the audit,
// rather than shipping and being discovered by a blank page.
func TestSecurityHeaders_CrossOriginIsolation(t *testing.T) {
	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for header, want := range map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	if got := rec.Header().Get("Cross-Origin-Embedder-Policy"); got != "" {
		t.Errorf("Cross-Origin-Embedder-Policy = %q, want it absent until every "+
			"cross-origin subresource is audited for CORP", got)
	}
}

func TestSecurityHeaders_XFrameOptions_DENY(t *testing.T) {
	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	xfo := rec.Header().Get("X-Frame-Options")
	if xfo != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", xfo)
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors") {
		t.Error("CSP missing frame-ancestors directive")
	}
}

func TestSecurityHeaders_ReferrerPolicy(t *testing.T) {
	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	rp := rec.Header().Get("Referrer-Policy")
	if rp != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", rp)
	}
}

func TestSecurityHeaders_ContentSniffingProtection(t *testing.T) {
	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestSecurityHeaders_CacheControl_AuthenticatedAPI(t *testing.T) {
	mw := apimw.SecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	cc := rec.Header().Get("Cache-Control")
	if cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// CSP with and without a nonce

func TestSecurityHeaders_CSP_Directives(t *testing.T) {
	// Without CSPNonce in chain, script-src/style-src are omitted entirely
	// because inline scripts are not allowed.
	// With CSPNonce in chain, nonce + strict-dynamic are used.

	t.Run("without_nonce_no_script_src", func(t *testing.T) {
		mw := apimw.SecurityHeaders(true)
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		csp := rec.Header().Get("Content-Security-Policy")

		// Without CSPNonce, no script-src/style-src (strict mode)
		if strings.Contains(csp, "script-src") {
			t.Error("CSP should NOT have script-src when no nonce is present")
		}
		if strings.Contains(csp, "style-src") {
			t.Error("CSP should NOT have style-src when no nonce is present")
		}
		// But base-uri and form-action are present
		if !strings.Contains(csp, "base-uri") {
			t.Error("CSP missing base-uri directive")
		}
		if !strings.Contains(csp, "form-action") {
			t.Error("CSP missing form-action directive")
		}
	})

	t.Run("with_nonce_has_nonce_and_strict_dynamic", func(t *testing.T) {
		// Wire CSPNonce middleware BEFORE SecurityHeaders
		nonceMW := apimw.CSPNonce()
		secMW := apimw.SecurityHeaders(true)
		handler := nonceMW(secMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		csp := rec.Header().Get("Content-Security-Policy")

		if !strings.Contains(csp, "'nonce-") {
			t.Error("CSP should have nonce when CSPNonce middleware is wired")
		}
		if !strings.Contains(csp, "'strict-dynamic'") {
			t.Error("CSP should have strict-dynamic when nonce is present")
		}
		if strings.Contains(csp, "'unsafe-inline'") {
			t.Errorf("CSP must NOT have unsafe-inline: %q", csp)
		}

		// Verify base-uri and form-action are present
		if !strings.Contains(csp, "base-uri") {
			t.Error("CSP missing base-uri directive")
		}
		if !strings.Contains(csp, "form-action") {
			t.Error("CSP missing form-action directive")
		}

		// X-CSP-Nonce response header is set
		xNonce := rec.Header().Get("X-CSP-Nonce")
		if xNonce == "" {
			t.Error("X-CSP-Nonce header is missing, so the frontend cannot read it")
		}
	})
}

func TestSecurityHeaders_AdminVsAPI_DifferentCSP(t *testing.T) {
	// Admin and API routers use different middleware.
	// Admin uses SecurityHeaders with CSPNonce for nonce-based script/style.
	// API uses APISecurityHeaders (JSON-only, no script-src).

	t.Run("admin_with_nonce_has_script_style", func(t *testing.T) {
		// Admin chain: CSPNonce -> SecurityHeaders
		nonceMW := apimw.CSPNonce()
		secMW := apimw.SecurityHeaders(true)
		handler := nonceMW(secMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		csp := rec.Header().Get("Content-Security-Policy")
		// Admin CSP with nonce should have script-src + style-src with nonce+strict-dynamic
		if !strings.Contains(csp, "script-src") {
			t.Error("Admin CSP should have script-src for SPA support")
		}
		if !strings.Contains(csp, "style-src") {
			t.Error("Admin CSP should have style-src for SPA support")
		}
		if !strings.Contains(csp, "'nonce-") {
			t.Error("Admin CSP should have nonce")
		}
	})

	t.Run("api_uses_APISecurityHeaders", func(t *testing.T) {
		mw := apimw.APISecurityHeaders(true)
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		csp := rec.Header().Get("Content-Security-Policy")

		// API CSP should NOT have script-src, style-src, img-src, font-src
		if strings.Contains(csp, "script-src") {
			t.Error("API CSP should NOT have script-src - API returns JSON only")
		}
		if strings.Contains(csp, "style-src") {
			t.Error("API CSP should NOT have style-src - API returns JSON only")
		}

		// API CSP should have the strict directives
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("API CSP should have default-src 'none': %q", csp)
		}
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Error("API CSP missing frame-ancestors 'none'")
		}

		// Baseline headers still present on API
		if got := rec.Header().Get("Permissions-Policy"); got == "" {
			t.Error("API Permissions-Policy header is missing")
		}
		if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Error("API Cross-Origin-Opener-Policy is missing or wrong")
		}
	})
}

// The public API router serves tenant media and public documents such as a
// sitemap to the crawlers a tenant site invites, so the refusal the admin
// surface sends stays off it.
func TestAPISecurityHeaders_DoesNotRefuseCrawlers(t *testing.T) {
	mw := apimw.APISecurityHeaders(true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if got := rec.Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("X-Robots-Tag = %q on the API router, want none", got)
	}
}
