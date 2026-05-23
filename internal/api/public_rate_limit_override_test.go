package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// admitted drives one wrapped route from a single client address and reports
// how many requests the limiter let through, which is the number a deployment
// sharing one source address actually cares about.
func admitted(t *testing.T, l *apimw.PublicEndpointRateLimiter, method, pattern string, requests int) int {
	t.Helper()
	h := l.Wrap(method, pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	count := 0
	for i := 0; i < requests; i++ {
		req := httptest.NewRequest(method, "/", nil)
		req.RemoteAddr = "198.51.100.9:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			count++
		}
	}
	return count
}

func limiterFor(t *testing.T, opts ...RouterOption) *apimw.PublicEndpointRateLimiter {
	t.Helper()
	o := &routerOptions{}
	for _, opt := range opts {
		opt(o)
	}
	l := buildPublicRateLimiter(o)
	if l == nil {
		t.Fatal("buildPublicRateLimiter returned nil")
	}
	t.Cleanup(l.Stop)
	return l
}

const loginRoute = "/api/admin/auth/login"

// A deployment that configures nothing gets the shipped caps: login admits its
// burst of 10 and refuses the rest.
func TestPublicRateLimiter_NoOverrideServesTheShippedCaps(t *testing.T) {
	l := limiterFor(t)

	if got := admitted(t, l, http.MethodPost, loginRoute, 40); got != 10 {
		t.Fatalf("login admitted %d of 40 requests, want the shipped burst of 10", got)
	}
}

// The same burst, once an operator has raised the cap. This is the setting's
// whole purpose: many callers behind one address are one caller to the limiter.
func TestPublicRateLimiter_OverrideAdmitsABurstTheDefaultRefuses(t *testing.T) {
	configs, global, err := apimw.ParsePublicRateLimits(
		[]string{"POST:" + loginRoute + "=200:2000"}, "2000:5000")
	if err != nil {
		t.Fatalf("parse overrides: %v", err)
	}
	l := limiterFor(t, WithPublicRateLimitOverrides(configs, global))

	if got := admitted(t, l, http.MethodPost, loginRoute, 40); got != 40 {
		t.Fatalf("login admitted %d of 40 requests, want all 40 under the raised cap", got)
	}
}

// Raising one route must not take the guard off the others.
func TestPublicRateLimiter_OverrideLeavesUnnamedRoutesAtTheirDefaults(t *testing.T) {
	configs, global, err := apimw.ParsePublicRateLimits(
		[]string{"POST:" + loginRoute + "=200:2000"}, "2000:5000")
	if err != nil {
		t.Fatalf("parse overrides: %v", err)
	}
	l := limiterFor(t, WithPublicRateLimitOverrides(configs, global))

	const mfaRoute = "/api/admin/auth/mfa-verify"
	if got := admitted(t, l, http.MethodPost, mfaRoute, 40); got != 10 {
		t.Fatalf("mfa-verify admitted %d of 40 requests, want its untouched burst of 10", got)
	}
}
