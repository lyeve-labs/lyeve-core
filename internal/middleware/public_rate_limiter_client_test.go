package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The public limiter counts a request against a client address, and which
// address that is decides whether the limit protects anyone.
//
// The address is resolved through the trusted proxies. Behind a load balancer
// RemoteAddr is the balancer, and counting against it would put every caller
// in one bucket, where the login limit could not tell an attacker apart from
// everyone else.

func limiterFor(t *testing.T, trusted []string) (*PublicEndpointRateLimiter, http.Handler) {
	t.Helper()
	var cidrs TrustedProxies
	for _, c := range trusted {
		_, n, err := net.ParseCIDR(c)
		require.NoError(t, err)
		cidrs = append(cidrs, n)
	}
	l := NewPublicEndpointRateLimiter(
		map[string]PublicRateLimitConfig{"POST:/probe": {Rate: 1, Burst: 2}},
		nil, cidrs,
	)
	h := l.Wrap(http.MethodPost, "/probe", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	))
	return l, h
}

func probe(h http.Handler, remoteAddr, xff string) int {
	r := httptest.NewRequest(http.MethodPost, "/probe", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

// Behind a trusted proxy, two clients get two allowances.
func TestPublicRateLimiter_CountsEachClientBehindATrustedProxy(t *testing.T) {
	_, h := limiterFor(t, []string{"10.0.0.0/8"})

	// One client burns its burst.
	assert.Equal(t, http.StatusOK, probe(h, "10.0.0.1:5000", "203.0.113.5"))
	assert.Equal(t, http.StatusOK, probe(h, "10.0.0.1:5000", "203.0.113.5"))
	assert.Equal(t, http.StatusTooManyRequests, probe(h, "10.0.0.1:5000", "203.0.113.5"),
		"the same client past its burst must be refused")

	// A different client behind the same proxy still has its own.
	assert.Equal(t, http.StatusOK, probe(h, "10.0.0.1:5000", "198.51.100.7"),
		"one caller must not be able to spend another's allowance")
}

// With no trusted proxy the header is a client's own claim and must be ignored,
// which is the spoof-proof default the limiter already documented.
func TestPublicRateLimiter_IgnoresTheHeaderFromAnUntrustedHop(t *testing.T) {
	_, h := limiterFor(t, nil)

	assert.Equal(t, http.StatusOK, probe(h, "203.0.113.9:5000", "1.1.1.1"))
	assert.Equal(t, http.StatusOK, probe(h, "203.0.113.9:5000", "2.2.2.2"))
	assert.Equal(t, http.StatusTooManyRequests, probe(h, "203.0.113.9:5000", "3.3.3.3"),
		"rotating X-Forwarded-For must not mint a new allowance when no proxy is trusted")
}

// A request that arrives from outside the trusted range carries a header nobody
// vouched for, so it is counted against the connection.
func TestPublicRateLimiter_UntrustedSourceIsCountedByItsConnection(t *testing.T) {
	_, h := limiterFor(t, []string{"10.0.0.0/8"})

	assert.Equal(t, http.StatusOK, probe(h, "203.0.113.9:5000", "1.1.1.1"))
	assert.Equal(t, http.StatusOK, probe(h, "203.0.113.9:5000", "2.2.2.2"))
	assert.Equal(t, http.StatusTooManyRequests, probe(h, "203.0.113.9:5000", "3.3.3.3"))
}
