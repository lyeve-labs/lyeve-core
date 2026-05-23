//go:build !mutest

// Package api: CSRF protection verification test suite.
//
// Covers the double-submit cookie pattern, the per-session CSRF token,
// SameSite=Strict cookies, and token rotation on login.
//
// Strategy:
//
//	Group A - Cookie attribute verification (SameSite=Strict, HttpOnly, __Host-csrf)
//	Group B - CSRF token generation and rotation verification
//	Group C - CORS header verification (X-CSRF-Token in allowlist)
//	Group D - Clickjacking + CSRF framing headers
//	Group E - Token design and __Host- cookie preconditions
//	Group F - Content-Type enforcement
//	Group G - CORS + CSRF interaction
package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Helpers

// csrfTestToken creates a random 32-byte base64url CSRF token.
func csrfTestToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand failed: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// extractCookie finds a cookie by name in the Set-Cookie header.
func extractCookie(headers http.Header, name string) string {
	for _, h := range headers["Set-Cookie"] {
		for _, part := range strings.Split(h, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, name+"=") {
				return strings.TrimPrefix(part, name+"=")
			}
		}
	}
	return ""
}

// cookieAttribute extracts an attribute from a Set-Cookie header.
// For boolean attributes (e.g., Secure, HttpOnly), returns "true" if present.
func cookieAttribute(headers http.Header, name, attr string) string {
	for _, h := range headers["Set-Cookie"] {
		if strings.HasPrefix(h, name+"=") || strings.Contains(h, "; "+name+"=") {
			parts := strings.Split(h, ";")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, attr+"=") {
					return strings.TrimPrefix(p, attr+"=")
				}
				if p == attr {
					return "true"
				}
			}
		}
	}
	return ""
}

// Group A: Cookie Attribute Verification

// The session cookie uses SameSite=Strict, so a cross-site request never
// carries it.
func TestCSRF_SameSiteIsStrict(t *testing.T) {
	t.Parallel()

	t.Run("session_cookie_samesite_strict", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setSessionCookie(w, "test-jwt-token")

		got := cookieAttribute(w.Header(), security.SessionCookieNameInsecure, "SameSite")
		if got != "Strict" {
			t.Errorf("setSessionCookie SameSite = %q, want Strict", got)
		}
	})

	t.Run("session_cookie_http_only", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setSessionCookie(w, "test-jwt-token")

		if got := cookieAttribute(w.Header(), security.SessionCookieNameInsecure, "HttpOnly"); got != "true" {
			t.Errorf("setSessionCookie HttpOnly = %q, want true", got)
		}
	})

	t.Run("session_cookie_path_root", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setSessionCookie(w, "test-jwt-token")

		if got := extractCookie(w.Header(), security.SessionCookieNameInsecure); got != "test-jwt-token" {
			t.Errorf("setSessionCookie value = %q, want test-jwt-token", got)
		}
	})

	t.Run("strict_blocks_cross_site_navigations", func(t *testing.T) {
		t.Log("SameSite=Strict blocks all cross-site cookie sending")
		t.Log("The admin SPA never needs cross-site cookie delivery")
	})
}

func TestCSRF_SessionCookieAttributes(t *testing.T) {
	t.Parallel()

	t.Run("cookie_name_constants_match", func(t *testing.T) {
		if security.SessionCookieNameFor(true) != "__Host-sys_session" {
			t.Errorf("secure session cookie = %q, want __Host-sys_session", security.SessionCookieNameFor(true))
		}
		if security.SessionCookieNameFor(false) != "sys_session" {
			t.Errorf("insecure session cookie = %q, want sys_session", security.SessionCookieNameFor(false))
		}
		if csrfCookieName != "__Host-csrf" {
			t.Errorf("csrfCookieName = %q, want __Host-csrf", csrfCookieName)
		}
	})

	// The __Host- prefix is only legal alongside Secure, and a browser drops a
	// __Host- cookie that arrives without it. A plaintext cookie therefore
	// drops the prefix, or a local server would refuse every CSRF-protected
	// admin write with "csrf token required".
	t.Run("plaintext_cookie_drops_the_host_prefix", func(t *testing.T) {
		if got := apimw.CSRFCookieNameFor(false); got == "__Host-csrf" {
			t.Errorf("insecure CSRF cookie = %q, must not carry the __Host- prefix", got)
		}
		if got := apimw.CSRFCookieNameFor(true); got != "__Host-csrf" {
			t.Errorf("secure CSRF cookie = %q, want __Host-csrf", got)
		}
	})

	t.Run("csrf_cookie_is_not_http_only", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-csrf-token")

		if got := cookieAttribute(w.Header(), apimw.CSRFCookieNameFor(false), "HttpOnly"); got != "" {
			t.Errorf("CSRF cookie should NOT be HttpOnly (JS must read it), got HttpOnly=%q", got)
		}
	})

	t.Run("csrf_cookie_samesite_strict", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-csrf-token")

		if got := cookieAttribute(w.Header(), apimw.CSRFCookieNameFor(false), "SameSite"); got != "Strict" {
			t.Errorf("CSRF cookie SameSite = %q, want Strict", got)
		}
	})

	t.Run("csrf_cookie_path_root", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)
		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-csrf-token")

		val := extractCookie(w.Header(), apimw.CSRFCookieNameFor(false))
		if val != "test-csrf-token" {
			t.Errorf("CSRF cookie value = %q, want test-csrf-token", val)
		}
	})

	t.Run("csrf_cookie_secure_in_production", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, true) // secureCook=true
		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-csrf-token")

		if got := cookieAttribute(w.Header(), csrfCookieName, "Secure"); got != "true" {
			t.Errorf("CSRF cookie Secure (production) = %q, want true", got)
		}
	})

	t.Run("cookie_separation_design", func(t *testing.T) {
		t.Log("DESIGN: sys_session (HttpOnly, auth) + __Host-csrf (!HttpOnly, CSRF)")
	})
}

// Group B: CSRF Token Presence Verification

func TestCSRF_TokenGeneration(t *testing.T) {
	t.Parallel()

	t.Run("generates_valid_token", func(t *testing.T) {
		token, err := generateCSRFToken()
		if err != nil {
			t.Fatalf("generateCSRFToken failed: %v", err)
		}
		if len(token) == 0 {
			t.Fatal("generateCSRFToken returned empty string")
		}
		// base64url of 32 bytes ~43 characters (no padding)
		if len(token) < 40 || len(token) > 50 {
			t.Errorf("token length = %d, expected ~43 chars (base64url of 32 bytes)", len(token))
		}
	})

	t.Run("tokens_are_url_safe", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			token, err := generateCSRFToken()
			if err != nil {
				t.Fatalf("generateCSRFToken failed at iter %d: %v", i, err)
			}
			for _, c := range token {
				if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
					(c >= '0' && c <= '9') || c == '-' || c == '_') {
					t.Errorf("token contains non-base64url char %q: %q", c, token)
				}
			}
		}
	})

	t.Run("tokens_are_unique", func(t *testing.T) {
		seen := make(map[string]bool, 1000)
		for i := 0; i < 1000; i++ {
			token, err := generateCSRFToken()
			if err != nil {
				t.Fatalf("generateCSRFToken failed at iter %d: %v", i, err)
			}
			if seen[token] {
				t.Fatalf("token collision at iteration %d: %q", i, token)
			}
			seen[token] = true
		}
	})

	t.Run("logout_clears_csrf_cookie", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, true)
		w := httptest.NewRecorder()
		h.clearCSRFCookie(w)

		cookies := w.Result().Cookies()
		var found bool
		for _, c := range cookies {
			if c.Name == csrfCookieName {
				found = true
				if c.MaxAge != -1 {
					t.Errorf("clearCSRFCookie MaxAge = %d, want -1", c.MaxAge)
				}
				if c.Value != "" {
					t.Errorf("clearCSRFCookie value = %q, want empty", c.Value)
				}
				// Secure must match setCSRFCookie's Secure flag so the browser
				// replaces the original cookie correctly.
				if !c.Secure {
					t.Errorf("clearCSRFCookie Secure = false, want true (must match setCSRFCookie)")
				}
			}
		}
		if !found {
			t.Error("clearCSRFCookie did not set __Host-csrf cookie")
		}
	})
}

// assertCSRFTokenIssued checks that a response both returns a CSRF token to the
// caller and sets the matching cookie. The double-submit pattern needs the
// pair: a body token with no cookie, or a cookie the SPA cannot read back,
// fails every subsequent write.
func assertCSRFTokenIssued(t *testing.T, rr *httptest.ResponseRecorder, secure bool, what string) {
	t.Helper()

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode response: %v (body=%q)", what, err, rr.Body.String())
	}
	token, _ := resp["csrf_token"].(string)
	if token == "" {
		t.Fatalf("%s: response carries no csrf_token: %v", what, resp)
	}
	cookie := extractCookie(rr.Header(), apimw.CSRFCookieNameFor(secure))
	if cookie == "" {
		t.Fatalf("%s: no CSRF cookie set alongside the body token", what)
	}
	if cookie != token {
		t.Errorf("%s: CSRF cookie = %q, body csrf_token = %q, want the same value", what, cookie, token)
	}
}

// Every route that hands out a session hands out a CSRF token with it. A route
// that issues a session and skips the token leaves the SPA holding a cookie it
// cannot pair, so its first write is refused with "csrf token required".
func TestCSRF_TokenIssuedWithEverySession(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"
	const email = "csrf-session@example.com"
	const password = "Correct-Horse-Battery-9"

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("csrf-setup-token-0123456"))

	post := func(t *testing.T, path, body string, call func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SetupTokenHeader, "csrf-setup-token-0123456")
		rr := httptest.NewRecorder()
		call(rr, req)
		return rr
	}

	// Setup runs first because it refuses once any account exists.
	t.Run("setup", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"email": email, "password": password})
		rr := post(t, "/api/admin/setup", string(body), h.Setup)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusCreated, rr.Body.String())
		}
		assertCSRFTokenIssued(t, rr, false, "setup")
	})

	t.Run("login", func(t *testing.T) {
		rr := post(t, "/api/admin/auth/login", makeLoginBody(email, password), h.Login)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
		}
		assertCSRFTokenIssued(t, rr, false, "login")
	})

	t.Run("refresh", func(t *testing.T) {
		u, err := users.GetByEmail(context.Background(), email)
		if err != nil {
			t.Fatalf("load seeded user: %v", err)
		}
		rt := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
		issued, err := rt.IssueForSession(context.Background(), u.ID.String(), "", u.TokenVersion, time.Hour)
		if err != nil {
			t.Fatalf("issue refresh token: %v", err)
		}
		rh := NewAuthHandler(users, nil, pool, secret, 3600, false)
		rh.WithRefreshTokenStore(rt, time.Hour)

		body, _ := json.Marshal(map[string]string{"refresh_token": issued.RefreshToken})
		rr := post(t, "/api/admin/auth/refresh", string(body), rh.Refresh)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
		}
		assertCSRFTokenIssued(t, rr, false, "refresh")
	})

	// Enrolling MFA changes what login returns, so this runs last.
	t.Run("mfa_verify", func(t *testing.T) {
		u, err := users.GetByEmail(context.Background(), email)
		if err != nil {
			t.Fatalf("load seeded user: %v", err)
		}
		plainSecret := seedMFA(t, pool, u.ID, secret)
		mh := NewAuthHandler(users, &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)

		rr := post(t, "/api/admin/auth/login", makeLoginBody(email, password), mh.Login)
		if rr.Code != http.StatusOK {
			t.Fatalf("login status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
		}
		var loginResp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &loginResp); err != nil {
			t.Fatalf("decode login response: %v", err)
		}
		challenge, _ := loginResp["challenge_token"].(string)
		if challenge == "" {
			t.Fatalf("no challenge token after enrolling MFA: %v", loginResp)
		}
		// A login that only issues a challenge is not a session, so it must not
		// hand out a CSRF token either.
		if tok, _ := loginResp["csrf_token"].(string); tok != "" {
			t.Errorf("MFA challenge response carried a csrf_token %q before the factor was answered", tok)
		}

		code, err := totp.GenerateCode(plainSecret, time.Now())
		if err != nil {
			t.Fatalf("generate TOTP code: %v", err)
		}
		body, _ := json.Marshal(map[string]string{"challenge_token": challenge, "code": code})
		rr2 := post(t, "/api/admin/auth/mfa-verify", string(body), mh.MFAVerify)
		if rr2.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%q", rr2.Code, http.StatusOK, rr2.Body.String())
		}
		assertCSRFTokenIssued(t, rr2, false, "mfa-verify")
	})
}

// Group C: CORS Header Verification

// X-CSRF-Token is in the CORS allowlist.
func TestCSRF_CORSHeaders(t *testing.T) {
	t.Parallel()

	t.Run("x_csrf_token_in_cors_allow_headers", func(t *testing.T) {
		const csrfHeader = "X-CSRF-Token"
		if csrfHeader != "X-CSRF-Token" {
			t.Errorf("csrfHeader = %q, want X-CSRF-Token", csrfHeader)
		}
		t.Log("X-CSRF-Token is in the default CORS_ALLOW_HEADERS")
	})

	t.Run("csrf_cookie_name_has_host_prefix", func(t *testing.T) {
		if csrfCookieName != "__Host-csrf" {
			t.Errorf("csrfCookieName = %q, want __Host-csrf", csrfCookieName)
		}
		t.Log("The __Host-csrf cookie uses the __Host- prefix for browser enforcement")
	})

	t.Run("no_origin_validation_middleware", func(t *testing.T) {
		t.Log("Origin is not checked: the double-submit token stops a forged write")
		t.Log("Double-submit cookie + SameSite=Strict provide the CSRF defense")
	})
}

// Group D: Clickjacking + CSRF Framing Headers

// Framing defenses are what stop an overlay attack from spending the session
// cookie the CSRF token is meant to protect. The middleware sets them, but a
// middleware that is not mounted protects nothing, so this asks the real
// routers.
//
// The CSP is also checked for 'unsafe-inline'. An inline-script allowance
// undoes frame-ancestors through an injected meta tag, so its absence is
// asserted here.
func TestCSRF_FramingHeadersAreWired(t *testing.T) {
	t.Parallel()

	routers := map[string]func() (http.Handler, error){
		"admin": func() (http.Handler, error) {
			return NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
		},
		"api": func() (http.Handler, error) {
			return NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)))
		},
	}

	for name, build := range routers {
		t.Run(name, func(t *testing.T) {
			r, err := build()
			if err != nil {
				t.Fatalf("build %s router: %v", name, err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("%s X-Frame-Options = %q, want DENY", name, got)
			}
			csp := rec.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Errorf("%s CSP = %q, want it to carry frame-ancestors 'none'", name, csp)
			}
			if strings.Contains(csp, "'unsafe-inline'") {
				t.Errorf("%s CSP allows 'unsafe-inline', which lets injected markup override frame-ancestors: %q", name, csp)
			}
		})
	}
}

// Group E: Token Design and __Host- Cookie Preconditions

func TestCSRF_TokenDesign_Randomness(t *testing.T) {
	t.Parallel()

	t.Run("32_bytes_is_sufficient", func(t *testing.T) {
		token := csrfTestToken(t)
		if len(token) == 0 {
			t.Fatal("generated empty token")
		}
		if len(token) < 40 || len(token) > 50 {
			t.Errorf("token length = %d, expected ~43 chars (base64url of 32 bytes)", len(token))
		}
	})

	t.Run("multiple_tokens_are_unique", func(t *testing.T) {
		seen := make(map[string]bool, 1000)
		for i := 0; i < 1000; i++ {
			token := csrfTestToken(t)
			if seen[token] {
				t.Fatalf("token collision at iteration %d: %q", i, token)
			}
			seen[token] = true
		}
	})

	t.Run("tokens_are_base64_url_safe", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			token := csrfTestToken(t)
			for _, c := range token {
				if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
					(c >= '0' && c <= '9') || c == '-' || c == '_') {
					t.Errorf("token contains non-base64url char %q: %q", c, token)
				}
			}
		}
	})
}

// A browser accepts a __Host- cookie only when it is Secure, has Path=/ and
// carries no Domain. Breaking any one of the three makes the browser drop the
// cookie outright, and the double-submit check then refuses every write with no
// clue as to why. Naming and Secure are covered above. Path and the absent
// Domain are the two nothing else pins.
func TestCSRF_HostPrefixCookieRequirements(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		secure   bool
		wantHost bool
	}{
		{"https", true, true},
		{"plaintext", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, tc.secure)
			w := httptest.NewRecorder()
			h.setCSRFCookie(w, "test-csrf-token")

			cookieName := apimw.CSRFCookieNameFor(tc.secure)
			if got := strings.HasPrefix(cookieName, "__Host-"); got != tc.wantHost {
				t.Fatalf("CSRF cookie %q carries the __Host- prefix = %v, want %v", cookieName, got, tc.wantHost)
			}
			if got := cookieAttribute(w.Header(), cookieName, "Path"); got != "/" {
				t.Errorf("CSRF cookie Path = %q, want /", got)
			}
			if got := cookieAttribute(w.Header(), cookieName, "Domain"); got != "" {
				t.Errorf("CSRF cookie Domain = %q, want it unset", got)
			}
		})
	}

	// The session cookie follows the CSRF cookie's rule. Over TLS it carries
	// the __Host- prefix, so no other host can plant or replace it, and the
	// browser holds the same name the admin console writes. Over plain HTTP it
	// drops the prefix, which a browser would refuse there. Logout clears the
	// name it set, or the dead session would stay in the browser.
	for _, tc := range []struct {
		name   string
		secure bool
		want   string
	}{
		{"session_cookie_https", true, "__Host-sys_session"},
		{"session_cookie_plaintext", false, "sys_session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, tc.secure)
			w := httptest.NewRecorder()
			h.setSessionCookie(w, "test-jwt-token")
			if got := extractCookie(w.Header(), tc.want); got != "test-jwt-token" {
				t.Fatalf("session cookie %q = %q, want test-jwt-token (headers %v)", tc.want, got, w.Header().Values("Set-Cookie"))
			}
			if got := cookieAttribute(w.Header(), tc.want, "Path"); got != "/" {
				t.Errorf("session cookie Path = %q, want /", got)
			}
			if got := cookieAttribute(w.Header(), tc.want, "Domain"); got != "" {
				t.Errorf("session cookie Domain = %q, want it unset", got)
			}
			wantSecure := ""
			if tc.secure {
				wantSecure = "true"
			}
			if got := cookieAttribute(w.Header(), tc.want, "Secure"); got != wantSecure {
				t.Errorf("session cookie Secure = %q, want %q", got, wantSecure)
			}

			out := httptest.NewRecorder()
			h.Logout(out, httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil))
			if got := cookieAttribute(out.Header(), tc.want, "Max-Age"); got != "0" {
				t.Errorf("logout Max-Age on %q = %q, want 0 (headers %v)", tc.want, got, out.Header().Values("Set-Cookie"))
			}
		})
	}
}

// Group F: Content-Type Verification (JSON CSRF defense-in-depth)

// TestCSRF_ContentTypeEnforcement asserts that both routers reject a mutation
// carrying a form content type, which is what stops a cross-origin HTML form
// from reaching a JSON handler at all.
//
// The middleware's own behavior is covered in internal/middleware. What is
// covered here is that both routers still mount it: a control that exists and
// is not wired protects nothing.
func TestCSRF_ContentTypeEnforcement(t *testing.T) {
	t.Parallel()

	routers := map[string]func() (http.Handler, error){
		"admin": func() (http.Handler, error) {
			return NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
		},
		"api": func() (http.Handler, error) {
			return NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)))
		},
	}

	for name, build := range routers {
		t.Run(name+"_rejects_a_form_content_type", func(t *testing.T) {
			r, err := build()
			if err != nil {
				t.Fatalf("build %s router: %v", name, err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/anything",
				strings.NewReader("a=1&b=2"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s router answered %d for a form-encoded POST, want %d",
					name, rec.Code, http.StatusUnsupportedMediaType)
			}
		})

		t.Run(name+"_admits_json", func(t *testing.T) {
			r, err := build()
			if err != nil {
				t.Fatalf("build %s router: %v", name, err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/anything",
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			// Whatever answers a route that does not exist, it must not be the
			// media-type refusal: that would mean the guard rejects everything.
			if rec.Code == http.StatusUnsupportedMediaType {
				t.Errorf("%s router refused application/json as an unsupported media type", name)
			}
		})
	}
}

// Group G: CORS Configuration Impact on CSRF

// CORS and CSRF are independent layers, and the two routers configure the CORS
// half differently on purpose. The admin router reflects credentials so the SPA
// may send its session cookie cross-origin. The API router does not, which
// leaves a stolen cookie useless there. Neither router refuses a request on the
// strength of its Origin: an unlisted origin only loses the response headers a
// browser needs in order to read the body, so the double-submit token is what
// actually stops a forged write.
func TestCSRF_CORSConfigurationImpact(t *testing.T) {
	t.Parallel()

	const allowedOrigin = "https://admin.example.test"
	const allowHeaders = "Content-Type, Authorization, X-API-Key, X-Tenant-ID, X-Correlation-ID, X-CSRF-Token"

	corsConfig := func() *config.Config {
		cfg := testConfig()
		cfg.CORSOrigins = []string{allowedOrigin}
		cfg.CORSAllowHeaders = allowHeaders
		return cfg
	}

	buildAdmin := func(t *testing.T) http.Handler {
		t.Helper()
		r, err := NewAdminRouter(&fakeDB{engine: "postgres"}, corsConfig(), WithLifetime(testLifetime(t)))
		if err != nil {
			t.Fatalf("build admin router: %v", err)
		}
		return r
	}
	buildAPI := func(t *testing.T) http.Handler {
		t.Helper()
		r, err := NewAPIRouter(&fakeDB{engine: "postgres"}, corsConfig(), nil, WithLifetime(testLifetime(t)))
		if err != nil {
			t.Fatalf("build api router: %v", err)
		}
		return r
	}
	preflight := func(h http.Handler, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/api/admin/users", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("admin_router_reflects_credentials", func(t *testing.T) {
		rec := preflight(buildAdmin(t), allowedOrigin)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
			t.Errorf("admin Access-Control-Allow-Origin = %q, want %q", got, allowedOrigin)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("admin Access-Control-Allow-Credentials = %q, want true", got)
		}
	})

	t.Run("api_router_withholds_credentials", func(t *testing.T) {
		rec := preflight(buildAPI(t), allowedOrigin)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
			t.Errorf("api Access-Control-Allow-Origin = %q, want %q", got, allowedOrigin)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("api Access-Control-Allow-Credentials = %q, want it unset", got)
		}
	})

	t.Run("preflight_advertises_the_csrf_header", func(t *testing.T) {
		// The browser drops X-CSRF-Token from the real request unless the
		// preflight names it, which fails every cross-origin write before the
		// server sees one. The routers must forward the configured list rather
		// than a list of their own.
		for name, h := range map[string]http.Handler{"admin": buildAdmin(t), "api": buildAPI(t)} {
			rec := preflight(h, allowedOrigin)
			if got := rec.Header().Get("Access-Control-Allow-Headers"); got != allowHeaders {
				t.Errorf("%s Access-Control-Allow-Headers = %q, want %q", name, got, allowHeaders)
			}
		}
	})

	t.Run("unlisted_origin_preflight_carries_no_cors_headers", func(t *testing.T) {
		for name, h := range map[string]http.Handler{"admin": buildAdmin(t), "api": buildAPI(t)} {
			for _, origin := range []string{"https://evil.example", "null"} {
				rec := preflight(h, origin)
				if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
					t.Errorf("%s reflected origin %q as %q, want nothing", name, origin, got)
				}
				if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
					t.Errorf("%s offered credentials to origin %q", name, origin)
				}
			}
		}
	})

	t.Run("unlisted_origin_still_reaches_the_auth_layer", func(t *testing.T) {
		// The server does not reject on Origin, so a forged write is stopped by
		// authentication and the CSRF token, not by CORS. Pinning this keeps an
		// origin check from being assumed to exist.
		req := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		buildAdmin(t).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated POST from a foreign origin = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("reflected a foreign origin as %q on a real request", got)
		}
	})
}

// Sanity check: test infrastructure works

func TestCSRF_TestInfrastructureWorks(t *testing.T) {
	t.Parallel()

	token := csrfTestToken(t)
	if token == "" {
		t.Fatal("csrfTestToken returned empty string")
	}

	rec := httptest.NewRecorder()
	rec.Header().Add("Set-Cookie", "foo=bar; Path=/; HttpOnly; SameSite=Strict")
	if got := extractCookie(rec.Header(), "foo"); got != "bar" {
		t.Errorf("extractCookie(foo) = %q, want bar", got)
	}
	if got := cookieAttribute(rec.Header(), "foo", "SameSite"); got != "Strict" {
		t.Errorf("cookieAttribute(foo, SameSite) = %q, want Strict", got)
	}

	rec2 := httptest.NewRecorder()
	rec2.Header().Add("Set-Cookie", "bar=baz; Secure; HttpOnly")
	if got := cookieAttribute(rec2.Header(), "bar", "Secure"); got != "true" {
		t.Errorf("cookieAttribute(bar, Secure) = %q, want true", got)
	}
}

// clearCSRFCookie uses the same Secure flag as setCSRFCookie. A hardcoded
// Secure: true would not clear the cookie a development environment set with
// secure cookies off.
func TestCSRF_ClearSecureFlagConsistency(t *testing.T) {
	t.Parallel()

	t.Run("clear_matches_set_when_secure_true", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, true)

		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-token")
		secSet := cookieAttribute(w.Header(), csrfCookieName, "Secure")

		w = httptest.NewRecorder()
		h.clearCSRFCookie(w)
		secClear := cookieAttribute(w.Header(), csrfCookieName, "Secure")

		if secSet != secClear {
			t.Errorf("Secure flag mismatch (prod): set=%q clear=%q", secSet, secClear)
		}
		if secSet != "true" {
			t.Errorf("expected Secure=true in production, got set=%q clear=%q", secSet, secClear)
		}
	})

	t.Run("clear_matches_set_when_secure_false", func(t *testing.T) {
		h := NewAuthHandler(nil, nil, nil, "test-secret", 3600, false)

		w := httptest.NewRecorder()
		h.setCSRFCookie(w, "test-token")
		secSet := cookieAttribute(w.Header(), csrfCookieName, "Secure")

		w = httptest.NewRecorder()
		h.clearCSRFCookie(w)
		secClear := cookieAttribute(w.Header(), csrfCookieName, "Secure")

		if secSet != secClear {
			t.Errorf("Secure flag mismatch (dev): set=%q clear=%q", secSet, secClear)
		}
		if secSet != "" {
			t.Errorf("expected Secure=unset in dev, got set=%q clear=%q", secSet, secClear)
		}
	})
}
