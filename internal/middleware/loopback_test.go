package middleware

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bucketSeen runs req through LoopbackCaller and returns the bucket it
// recorded.
func bucketSeen(req *http.Request) string {
	var got string
	LoopbackCaller()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = loopbackBucketFromCtx(r)
	})).ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func markedRequest(local net.Addr, remote string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
	req.RemoteAddr = remote
	req.Header.Set("Authorization", "Bearer token-acme")
	req.Header.Set("X-Tenant-ID", "acme")
	MarkLoopback(req)
	return req
}

func TestLoopbackCaller_MarkFromOwnAddressNamesTenantBucket(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 3002}
	got := bucketSeen(markedRequest(local, "10.0.0.5:51000"))
	require.NotEmpty(t, got)
	assert.Contains(t, got, ":acme:")

	other := markedRequest(local, "10.0.0.5:51001")
	other.Header.Set("Authorization", "Bearer token-other")
	assert.NotEqual(t, got, bucketSeen(other), "two credentials in one tenant count apart")
}

func TestLoopbackCaller_MarkFromAnotherAddressKeepsAddressBucket(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 3002}
	assert.Empty(t, bucketSeen(markedRequest(local, "203.0.113.9:40000")),
		"the right mark from another host must not name a bucket")

	noLocal := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	MarkLoopback(noLocal)
	assert.Empty(t, bucketSeen(noLocal), "a request with no known listener address must not name a bucket")
}

func TestPerTenantRateLimiter_LoopbackCallsCountByTenant(t *testing.T) {
	h := LoopbackCaller()(PerTenantRateLimiter(0.001, 2)(http.HandlerFunc(okHandler)))
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3002}
	call := func(tenant string) int {
		req := markedRequest(local, "127.0.0.1:50000")
		req.Header.Set("X-Tenant-ID", tenant)
		req.Header.Set("Authorization", "Bearer token-"+tenant)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusOK, call("acme"))
	require.Equal(t, http.StatusOK, call("acme"))
	require.Equal(t, http.StatusTooManyRequests, call("acme"))
	assert.Equal(t, http.StatusOK, call("globex"))
}

func TestPublicEndpointRateLimiter_LoopbackCallsCountByTenant(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		pattern string
		configs map[string]PublicRateLimitConfig
		global  *PublicRateLimitConfig
		custom  routeLimitTable
	}{
		{
			name:    "route limit",
			method:  http.MethodGet,
			pattern: "/pub/own",
			configs: map[string]PublicRateLimitConfig{"GET:/pub/own": {Rate: 0.001, Burst: 1}},
		},
		{
			name:    "operator limit",
			method:  http.MethodGet,
			pattern: "/pub/custom",
			global:  &PublicRateLimitConfig{Rate: 0.001, Burst: 100},
			custom:  routeLimitTable{"GET /pub/custom": {Rate: 0.001, Burst: 1}},
		},
		{
			name:    "global cap",
			method:  http.MethodGet,
			pattern: "/pub/general",
			global:  &PublicRateLimitConfig{Rate: 0.001, Burst: 1},
		},
		{
			name:    "credential route",
			method:  http.MethodPost,
			pattern: "/api/v1/auth/token",
			configs: map[string]PublicRateLimitConfig{"POST:/api/v1/auth/token": {Rate: 0.001, Burst: 1}},
		},
	}
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3002}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.custom != nil {
				t.Cleanup(core.RegisterPublicRouteLimitResolver(tc.custom))
			}
			limiter := NewPublicEndpointRateLimiter(tc.configs, tc.global, nil)
			h := LoopbackCaller()(limiter.Wrap(tc.method, tc.pattern, http.HandlerFunc(okHandler)))
			call := func(tenant, mark string) int {
				req := httptest.NewRequest(tc.method, tc.pattern, nil)
				req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
				req.RemoteAddr = "127.0.0.1:50000"
				req.Header.Set("X-Tenant-ID", tenant)
				req.Header.Set("Authorization", "Bearer token-"+tenant)
				switch mark {
				case "own":
					MarkLoopback(req)
				case "wrong":
					req.Header.Set(LoopbackHeader, "not-the-mark")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Code
			}
			require.Equal(t, http.StatusOK, call("acme", "own"))
			require.Equal(t, http.StatusTooManyRequests, call("acme", "own"), "a tenant's calls spend its own bucket")
			assert.Equal(t, http.StatusOK, call("globex", "own"), "another tenant's calls keep their bucket")
			assert.Equal(t, http.StatusOK, call("acme", ""), "an unmarked call keeps the address bucket, unspent by loopback calls")
			assert.Equal(t, http.StatusTooManyRequests, call("globex", "wrong"), "a wrong mark shares the address bucket")
		})
	}
}
