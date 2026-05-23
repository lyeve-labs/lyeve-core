package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// The guards in applyBuiltinMiddleware refuse without calling next: MaxBodySize
// answers 413, RateLimiter 429, PathSanitize 400. A middleware registered after
// them never runs on a refusal, so the security headers are registered ahead
// of them, or the responses an attacker can provoke cheapest would go out
// without nosniff or a frame policy.
func refusalRouter(t *testing.T, first ...func(http.Handler) http.Handler) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	applyBuiltinMiddleware(r, &config.Config{
		MaxBodyBytes:   16,
		RateLimitRPS:   1,
		RateLimitBurst: 1,
	}, nil, first...)
	r.Post("/api/v1/thing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	return r
}

func assertHardened(t *testing.T, res *httptest.ResponseRecorder, status int, what string) {
	t.Helper()
	require.Equal(t, status, res.Code, what+": status")
	assert.Equal(t, "nosniff", res.Header().Get("X-Content-Type-Options"), what+": X-Content-Type-Options")
	assert.NotEmpty(t, res.Header().Get("X-Frame-Options"), what+": X-Frame-Options")
	assert.NotEmpty(t, res.Header().Get("Referrer-Policy"), what+": Referrer-Policy")
}

func TestBuiltinMiddleware_OversizeBodyRefusalCarriesSecurityHeaders(t *testing.T) {
	r := refusalRouter(t, apimw.APISecurityHeaders(false))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", strings.NewReader(strings.Repeat("x", 4096)))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	assertHardened(t, res, http.StatusRequestEntityTooLarge, "413 from MaxBodySize")
}

func TestBuiltinMiddleware_RateLimitRefusalCarriesSecurityHeaders(t *testing.T) {
	r := refusalRouter(t, apimw.APISecurityHeaders(false))

	// Burst is 1, so the second call from the same address is refused.
	var res *httptest.ResponseRecorder
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:1234"
		res = httptest.NewRecorder()
		r.ServeHTTP(res, req)
	}

	assertHardened(t, res, http.StatusTooManyRequests, "429 from RateLimiter")
}

func TestBuiltinMiddleware_PathSanitizeRefusalCarriesSecurityHeaders(t *testing.T) {
	r := refusalRouter(t, apimw.APISecurityHeaders(false))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.URL.Path = "/api/v1/th\x00ing"
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	assertHardened(t, res, http.StatusBadRequest, "400 from PathSanitize")
}

// The admin chain needs CSPNonce ahead of SecurityHeaders for the nonce-based
// CSP, so both move together. A refusal must still carry the strict policy.
func TestBuiltinMiddleware_AdminRefusalCarriesNonceBackedCSP(t *testing.T) {
	r := refusalRouter(t, apimw.CSPNonce(), apimw.SecurityHeaders(false))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", strings.NewReader(strings.Repeat("x", 4096)))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)

	assertHardened(t, res, http.StatusRequestEntityTooLarge, "413 on the admin chain")
	csp := res.Header().Get("Content-Security-Policy")
	require.NotEmpty(t, csp, "413 must carry a CSP")
	assert.Contains(t, csp, "nonce-", "the CSP on a refusal must still be nonce-backed")
}
