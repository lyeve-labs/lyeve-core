package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBucketAddress(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":                 "203.0.113.9",
		"::ffff:203.0.113.9":          "203.0.113.9",
		"2001:db8:1:2:aaaa:bbbb:cc:1": "2001:db8:1:2::/64",
		"2001:db8:1:2::ffff":          "2001:db8:1:2::/64",
		"2001:db8:1:3::1":             "2001:db8:1:3::/64",
		"not an address":              "not an address",
	} {
		assert.Equal(t, want, bucketAddress(in), in)
	}
}

// One IPv6 host picks any address in its /64. Keyed on the full address it
// would have a fresh bucket per request. Keyed on the prefix it has one. Each
// of the three limiters builds its key on its own path, so each is exercised.
func TestRateLimiters_OneIPv6HostSharesItsPrefixBucket(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	public := NewPublicEndpointRateLimiter(map[string]PublicRateLimitConfig{
		"POST:/auth/login": {Rate: 1, Burst: 1},
	}, nil, nil)
	limiters := map[string]struct {
		h      http.Handler
		method string
		ctx    func(*http.Request) *http.Request
	}{
		"global": {RateLimiter(1, 1)(ok), http.MethodGet, nil},
		"per tenant": {PerTenantRateLimiter(1, 1)(ok), http.MethodGet, func(r *http.Request) *http.Request {
			return r.WithContext(context.WithValue(r.Context(), tenantKey{}, "acme"))
		}},
		"public endpoint": {public.Wrap("POST", "/auth/login", ok), http.MethodPost, nil},
	}
	for name, l := range limiters {
		t.Run(name, func(t *testing.T) {
			serve := func(addr string) int {
				r := httptest.NewRequest(l.method, "/auth/login", nil)
				r.RemoteAddr = "[" + addr + "]:443"
				if l.ctx != nil {
					r = l.ctx(r)
				}
				rec := httptest.NewRecorder()
				l.h.ServeHTTP(rec, r)
				return rec.Code
			}
			assert.Equal(t, http.StatusOK, serve("2001:db8:1:2::1"))
			assert.Equal(t, http.StatusTooManyRequests, serve("2001:db8:1:2::2"), "another address in the same /64 is the same client")
			assert.Equal(t, http.StatusOK, serve("2001:db8:1:3::1"), "the next /64 is another client")
		})
	}
}
