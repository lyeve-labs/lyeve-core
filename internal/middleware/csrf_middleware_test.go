// Package middleware_test covers the CSRF middleware: the double-submit cookie
// design (__Host-csrf plus X-CSRF-Token), how tokens are compared, and the
// safe-method exemptions enforced by CSRFCheck.
//
// Run: go test ./internal/middleware/ -run TestCSRF -count=1 -race
package middleware_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// serveCSRF drives one request through CSRFCheck and returns what came back.
// The wrapped handler answers 200 and writes nothing, so any other status or
// any body at all was produced by the middleware.
func serveCSRF(t *testing.T, r *http.Request) (int, string) {
	t.Helper()
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// csrfMutation builds a POST carrying the given cookies and nothing else.
func csrfMutation(cookies ...*http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

func namedCookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value}
}

// Cookie Design Validation

// The double-submit pattern rests on the cookie name as much as on its value.
// Over TLS the __Host- prefix is what stops a sibling subdomain planting a
// cookie the middleware would then accept as its own. Over plain HTTP that
// prefix is illegal, so the server falls back to an unprefixed name and
// CSRFCheck reads either one. The order it reads them in is the security
// property: the prefixed cookie has to win, or a planted unprefixed cookie
// decides the comparison on a deployment that has both.
func TestCSRF_CookieDesign(t *testing.T) {
	t.Run("cookie_name_tracks_the_connection_scheme", func(t *testing.T) {
		if got := middleware.CSRFCookieNameFor(true); got != "__Host-csrf" {
			t.Errorf("CSRFCookieNameFor(true) = %q, want __Host-csrf", got)
		}
		if got := middleware.CSRFCookieNameFor(false); strings.HasPrefix(got, "__Host-") {
			t.Errorf("CSRFCookieNameFor(false) = %q, must not carry the __Host- prefix", got)
		}
	})

	t.Run("unprefixed_cookie_is_accepted", func(t *testing.T) {
		req := csrfMutation(
			namedCookie("sys_session", "jwt"),
			namedCookie(middleware.CSRFCookieNameFor(false), "plaintext-token"),
		)
		req.Header.Set("X-CSRF-Token", "plaintext-token")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("plaintext cookie name = %d %s, want 200", code, body)
		}
	})

	t.Run("prefixed_cookie_outranks_the_unprefixed_one", func(t *testing.T) {
		both := func(header string) *http.Request {
			req := csrfMutation(
				namedCookie("sys_session", "jwt"),
				namedCookie("__Host-csrf", "prefixed-token"),
				namedCookie(middleware.CSRFCookieNameFor(false), "planted-token"),
			)
			req.Header.Set("X-CSRF-Token", header)
			return req
		}

		if code, body := serveCSRF(t, both("prefixed-token")); code != http.StatusOK {
			t.Errorf("header matching the __Host- cookie = %d %s, want 200", code, body)
		}
		if code, _ := serveCSRF(t, both("planted-token")); code != http.StatusForbidden {
			t.Errorf("header matching the unprefixed cookie = %d, want 403", code)
		}
	})
}

// Origin and Referer: No Validation Runs

// CSRF protection is the double-submit pair alone. CSRFCheck decides on the
// cookie and the header, and no Origin or Referer check runs, so a mutation
// carrying a matching pair is served whatever site it claims to come from, the
// null origin of a sandboxed frame included. These cases pin that, so adding
// an origin check has to update this test.
func TestCSRF_OriginValidationMiddleware(t *testing.T) {
	valid := func() *http.Request {
		req := csrfMutation(
			namedCookie("sys_session", "jwt"),
			namedCookie("__Host-csrf", "matching-token"),
		)
		req.Header.Set("X-CSRF-Token", "matching-token")
		return req
	}

	t.Run("foreign_origin_is_served", func(t *testing.T) {
		req := valid()
		req.Header.Set("Origin", "https://evil.example")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("foreign Origin = %d %s, want 200 (no origin check runs)", code, body)
		}
	})

	t.Run("null_origin_is_served", func(t *testing.T) {
		req := valid()
		req.Header.Set("Origin", "null")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("null Origin = %d %s, want 200 (no origin check runs)", code, body)
		}
	})

	t.Run("foreign_referer_is_served", func(t *testing.T) {
		req := valid()
		req.Header.Set("Referer", "https://evil.example/page")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("foreign Referer = %d %s, want 200 (no referer fallback runs)", code, body)
		}
	})

	t.Run("foreign_origin_does_not_reject_a_bearer_request", func(t *testing.T) {
		req := csrfMutation()
		req.Header.Set("Authorization", "Bearer some-jwt")
		req.Header.Set("Origin", "https://evil.example")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("bearer request with foreign Origin = %d %s, want 200", code, body)
		}
	})
}

// Edge Cases

// The comparison is over the cookie text and the header text exactly as they
// arrived. Nothing is decoded, so two encodings of the same 32 bytes do not
// match, and nothing is normalized beyond trimming the header. These cases fix
// which malformed inputs land on "required" and which land on "mismatch",
// since the two answers tell a caller different things about what to retry.
func TestCSRF_EdgeCases(t *testing.T) {
	const token = "ZG91YmxlLXN1Ym1pdC10b2tlbi12YWx1ZS0zMi1ieXRl"

	t.Run("duplicate_cookies_use_the_first", func(t *testing.T) {
		dup := func(header string) *http.Request {
			req := csrfMutation(
				namedCookie("sys_session", "jwt"),
				namedCookie("__Host-csrf", "first-value"),
				namedCookie("__Host-csrf", "second-value"),
			)
			req.Header.Set("X-CSRF-Token", header)
			return req
		}

		if code, body := serveCSRF(t, dup("first-value")); code != http.StatusOK {
			t.Errorf("header matching the first cookie = %d %s, want 200", code, body)
		}
		if code, _ := serveCSRF(t, dup("second-value")); code != http.StatusForbidden {
			t.Errorf("header matching the second cookie = %d, want 403", code)
		}
	})

	t.Run("truncated_token_is_a_mismatch", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "jwt"), namedCookie("__Host-csrf", token))
		req.Header.Set("X-CSRF-Token", token[:20])

		code, body := serveCSRF(t, req)
		if code != http.StatusForbidden {
			t.Errorf("truncated token = %d, want 403", code)
		}
		if body != `{"error":"csrf token mismatch"}` {
			t.Errorf("truncated token body = %q, want a mismatch", body)
		}
	})

	t.Run("header_whitespace_is_trimmed", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "jwt"), namedCookie("__Host-csrf", token))
		req.Header.Set("X-CSRF-Token", "  "+token+"\t")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("whitespace-wrapped header = %d %s, want 200", code, body)
		}
	})

	t.Run("whitespace_only_header_reads_as_missing", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "jwt"), namedCookie("__Host-csrf", token))
		req.Header.Set("X-CSRF-Token", "   ")

		code, body := serveCSRF(t, req)
		if code != http.StatusForbidden {
			t.Errorf("whitespace-only header = %d, want 403", code)
		}
		if body != `{"error":"csrf token required"}` {
			t.Errorf("whitespace-only header body = %q, want the missing-token answer", body)
		}
	})

	t.Run("base64_padding_is_not_normalized", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "jwt"), namedCookie("__Host-csrf", token))
		req.Header.Set("X-CSRF-Token", token+"=")

		code, body := serveCSRF(t, req)
		if code != http.StatusForbidden {
			t.Errorf("padded token = %d, want 403", code)
		}
		if body != `{"error":"csrf token mismatch"}` {
			t.Errorf("padded token body = %q, want a mismatch", body)
		}
	})

	t.Run("empty_cookie_value_reads_as_missing", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "jwt"), namedCookie("__Host-csrf", ""))
		req.Header.Set("X-CSRF-Token", token)

		code, body := serveCSRF(t, req)
		if code != http.StatusForbidden {
			t.Errorf("empty cookie value = %d, want 403", code)
		}
		if body != `{"error":"csrf token required"}` {
			t.Errorf("empty cookie value body = %q, want the missing-token answer", body)
		}
	})
}

// CSRFCheck End to End

func TestCSRF_Behavioral_SafeMethodsPassThrough(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "TRACE"} {
		t.Run(method, func(t *testing.T) {
			handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(method, "/api/admin/schemas", nil)
			// Add session cookie so we're not just skipping due to no cookie
			req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("%s: expected 200, got %d", method, rec.Code)
			}
		})
	}
}

func TestCSRF_Behavioral_MutatingMethodsRequireCSRF(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method+"_no_session_cookie_passes", func(t *testing.T) {
			handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(method, "/api/admin/schemas", nil)
			// No session cookie: should pass through (Bearer token auth)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("%s without session cookie: expected 200, got %d", method, rec.Code)
			}
		})

		t.Run(method+"_session_cookie_no_csrf_header_rejected", func(t *testing.T) {
			handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(method, "/api/admin/schemas", nil)
			req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
			// No __Host-csrf cookie, No X-CSRF-Token header
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("%s with session cookie but no CSRF token: expected 403, got %d", method, rec.Code)
			}
		})
	}
}

func TestCSRF_Behavioral_TokenMismatchRejected(t *testing.T) {
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "real-token"})
	req.Header.Set("X-CSRF-Token", "fake-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched tokens: expected 403, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"error":"csrf token mismatch"}` {
		t.Errorf("mismatched tokens: expected 'csrf token mismatch', got %q", body)
	}
}

func TestCSRF_Behavioral_ValidTokensPass(t *testing.T) {
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("valid tokens: expected 200, got %d", rec.Code)
	}
}

func TestCSRF_Behavioral_MissingCSRFCookie(t *testing.T) {
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.Header.Set("X-CSRF-Token", "some-token")
	// No __Host-csrf cookie
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("missing CSRF cookie: expected 403, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"error":"csrf token required"}` {
		t.Errorf("missing CSRF cookie: expected 'csrf token required', got %q", body)
	}
}

func TestCSRF_Behavioral_MissingCSRFHeader(t *testing.T) {
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "some-token"})
	// No X-CSRF-Token header
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("missing CSRF header: expected 403, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"error":"csrf token required"}` {
		t.Errorf("missing CSRF header: expected 'csrf token required', got %q", body)
	}
}

func TestCSRF_Behavioral_BearerTokenBypass(t *testing.T) {
	// Requests with Bearer token + no sys_session cookie pass through
	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas", nil)
	req.Header.Set("Authorization", "Bearer some-jwt")
	// No session cookie at all
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Bearer-only request: expected 200, got %d", rec.Code)
	}
}

// A session cookie alongside a bearer token still needs the double-submit
// token. The bearer skip exists because a browser never attaches Authorization
// on its own. A request that also carries the session cookie is exactly the one
// a browser would be tricked into sending, so the header must not buy it an
// exemption.
func TestCSRF_Behavioral_BearerDoesNotExemptACookieRequest(t *testing.T) {
	t.Run("no_token_is_still_rejected", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "fake-jwt"))
		req.Header.Set("Authorization", "Bearer some-jwt")

		code, body := serveCSRF(t, req)
		if code != http.StatusForbidden {
			t.Errorf("cookie plus bearer, no CSRF token = %d, want 403", code)
		}
		if body != `{"error":"csrf token required"}` {
			t.Errorf("cookie plus bearer, no CSRF token body = %q, want the missing-token answer", body)
		}
	})

	t.Run("matching_token_passes", func(t *testing.T) {
		req := csrfMutation(namedCookie("sys_session", "fake-jwt"), namedCookie("__Host-csrf", "paired-token"))
		req.Header.Set("Authorization", "Bearer some-jwt")
		req.Header.Set("X-CSRF-Token", "paired-token")

		if code, body := serveCSRF(t, req); code != http.StatusOK {
			t.Errorf("cookie plus bearer with a matching token = %d %s, want 200", code, body)
		}
	})
}

// A browser on a TLS deployment carries __Host-sys_session. That is a session
// cookie like the plain one, so a write that brings it is held to the
// double-submit check, or a cross-site form could ride the session.
func TestCSRFCheck_PrefixedSessionCookieIsCookieAuth(t *testing.T) {
	t.Parallel()

	handler := middleware.CSRFCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	bare := httptest.NewRequest(http.MethodPost, "/api/admin/media", nil)
	bare.AddCookie(&http.Cookie{Name: "__Host-sys_session", Value: "jwt"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, bare)
	if rec.Code != http.StatusForbidden {
		t.Errorf("prefixed session cookie without a CSRF pair: got %d, want 403", rec.Code)
	}

	paired := httptest.NewRequest(http.MethodPost, "/api/admin/media", nil)
	paired.AddCookie(&http.Cookie{Name: "__Host-sys_session", Value: "jwt"})
	paired.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: "pair"})
	paired.Header.Set("X-CSRF-Token", "pair")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, paired)
	if rec.Code != http.StatusOK {
		t.Errorf("prefixed session cookie with a matching CSRF pair: got %d, want 200", rec.Code)
	}
}
