package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type routeLimitTable map[string]core.RouteRateLimit

func (m routeLimitTable) PublicRouteLimit(endpoint string) (core.RouteRateLimit, bool) {
	l, ok := m[endpoint]
	return l, ok
}

// hits sends n requests from one address and counts the ones answered 200.
func hits(h http.Handler, method, path string, n int) int {
	ok := 0
	for i := 0; i < n; i++ {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "10.9.0.1:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			ok++
		}
	}
	return ok
}

func TestPublicEndpointRateLimiter_AnOperatorLimitReplacesTheGeneralOne(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{}, &PublicRateLimitConfig{Rate: 0.001, Burst: 3}, nil)
	raised := limiter.Wrap("GET", "/general/raised", http.HandlerFunc(okHandler))
	lowered := limiter.Wrap("GET", "/general/lowered", http.HandlerFunc(okHandler))
	plain := limiter.Wrap("GET", "/general/plain", http.HandlerFunc(okHandler))
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"GET /general/raised":  {Rate: 0.001, Burst: 20},
		"GET /general/lowered": {Rate: 0.001, Burst: 1},
	}))

	assert.Equal(t, 20, hits(raised, "GET", "/general/raised", 30), "a raised route passes its own limit, past the general one")
	assert.Equal(t, 1, hits(lowered, "GET", "/general/lowered", 5), "a lowered route stops at its own limit")
	assert.Equal(t, 3, hits(plain, "GET", "/general/plain", 10), "a route with no operator limit keeps the general one, unspent by the other two")
}

func TestPublicEndpointRateLimiter_ARouteWithItsOwnLimitIgnoresAnOperatorLimit(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{
		"POST:/own/login": {Rate: 0.001, Burst: 2},
	}, &PublicRateLimitConfig{Rate: 0.001, Burst: 100}, nil)
	login := limiter.Wrap("POST", "/own/login", http.HandlerFunc(okHandler))
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"POST /own/login": {Rate: 1000, Burst: 1000},
	}))

	assert.Equal(t, 2, hits(login, "POST", "/own/login", 10), "a credential route keeps its own limit")
}

func TestPublicEndpointRateLimiter_AnOperatorLimitAppliesWithNoGlobalCap(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{}, nil, nil)
	h := limiter.Wrap("GET", "/nocap/limited", http.HandlerFunc(okHandler))
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"GET /nocap/limited": {Rate: 0.001, Burst: 2},
	}))
	assert.Equal(t, 2, hits(h, "GET", "/nocap/limited", 5))
}

func TestPublicEndpointRateLimiter_VerifyAcceptsRoutesOnTheGeneralLimit(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{
		"POST:/v/login": {Rate: 5, Burst: 10},
	}, &PublicRateLimitConfig{Rate: 50, Burst: 100}, nil)
	limiter.Wrap("POST", "/v/login", http.HandlerFunc(okHandler))
	limiter.Wrap("GET", "/v/search", http.HandlerFunc(okHandler))
	limiter.Wrap("GET", "/v/brand", http.HandlerFunc(okHandler))

	assert.NoError(t, limiter.Verify(), "with a global cap a route without its own limit is on the general limit, not a gap")
	assert.Equal(t, []string{"GET:/v/brand", "GET:/v/search"}, limiter.GeneralRoutes())
}

func TestPublicEndpointRateLimiter_VerifyNamesRoutesWithNoLimitAtAll(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{
		"POST:/u/login": {Rate: 5, Burst: 10},
	}, nil, nil)
	limiter.Wrap("POST", "/u/login", http.HandlerFunc(okHandler))
	limiter.Wrap("GET", "/u/search", http.HandlerFunc(okHandler))

	err := limiter.Verify()
	require.Error(t, err, "with no global cap the route is unlimited")
	assert.Contains(t, err.Error(), "GET:/u/search")
	assert.NotContains(t, err.Error(), "POST:/u/login")
}

func TestPublicEndpointRateLimiter_ACredentialRouteNeverTakesAnOperatorLimit(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{}, &PublicRateLimitConfig{Rate: 0.001, Burst: 2}, nil)
	h := limiter.Wrap("POST", "/api/v1/auth/magic", http.HandlerFunc(okHandler))
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"POST /api/v1/auth/magic": {Rate: 1000, Burst: 1000},
	}))
	assert.Equal(t, 2, hits(h, "POST", "/api/v1/auth/magic", 10), "a credential route stays on the general cap whatever a resolver answers")
	assert.NotContains(t, limiter.GeneralRoutes(), "POST:/api/v1/auth/magic")
	require.Error(t, limiter.Verify(), "and Verify names it, so its missing limit is found at boot")
}

func TestPublicEndpointRateLimiter_HeadFollowsTheGetRoutesOperatorLimit(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{}, &PublicRateLimitConfig{Rate: 0.001, Burst: 100}, nil)
	get := limiter.Wrap("GET", "/api/v1/media/{id}/transform", http.HandlerFunc(okHandler))
	head := limiter.Wrap("HEAD", "/api/v1/media/{id}/transform", http.HandlerFunc(okHandler))
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"GET /api/v1/media/{id}/transform": {Rate: 0.001, Burst: 3},
	}))
	assert.Equal(t, 2, hits(get, "GET", "/api/v1/media/x/transform", 2))
	assert.Equal(t, 1, hits(head, "HEAD", "/api/v1/media/x/transform", 5), "HEAD spends the GET route's bucket")
}

func TestPublicEndpointRateLimiter_HeadFollowsTheGetRoutesOwnLimit(t *testing.T) {
	limiter := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{
		"GET:/api/v1/files/{id}": {Rate: 0.001, Burst: 3},
	}, &PublicRateLimitConfig{Rate: 0.001, Burst: 100}, nil)
	get := limiter.Wrap("GET", "/api/v1/files/{id}", http.HandlerFunc(okHandler))
	head := limiter.Wrap("HEAD", "/api/v1/files/{id}", http.HandlerFunc(okHandler))
	// A limit stored while the route ran on the general limit no longer
	// applies once the GET route has one of its own, for either method.
	t.Cleanup(core.RegisterPublicRouteLimitResolver(routeLimitTable{
		"GET /api/v1/files/{id}": {Rate: 1000, Burst: 1000},
	}))
	assert.Equal(t, 2, hits(get, "GET", "/api/v1/files/x", 2))
	assert.Equal(t, 1, hits(head, "HEAD", "/api/v1/files/x", 5), "HEAD spends the GET route's own bucket, not the stored raise")
	assert.NotContains(t, limiter.GeneralRoutes(), "HEAD:/api/v1/files/{id}")
	require.NoError(t, limiter.Verify())
}

func TestCredentialRoute_MatchesEveryAuthSegment(t *testing.T) {
	for _, p := range []string{"/api/admin/auth/login", "/api/v1/auth/token", "/auth/passwordless/request", "/api/admin/auth"} {
		assert.True(t, CredentialRoute(p), p)
	}
	for _, p := range []string{"/api/v1/authors", "/api/v1/search/{schema}", "/api/admin/auth-settings", "/api/v1/localization/resolve"} {
		assert.False(t, CredentialRoute(p), p)
	}
}
