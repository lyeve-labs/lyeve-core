package middleware

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

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
