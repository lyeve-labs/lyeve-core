package middleware_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// auditCapture records every call to the audit log function.
type auditCapture struct {
	mu  sync.Mutex
	log []auditRecord
}

type auditRecord struct {
	apiKeyID string
	method   string
	path     string
	status   int
	ip       string
}

func (c *auditCapture) logFn() func(string, string, string, int, string) {
	return func(apiKeyID, method, path string, status int, ip string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.log = append(c.log, auditRecord{apiKeyID, method, path, status, ip})
	}
}

func (c *auditCapture) records() []auditRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]auditRecord{}, c.log...)
}

// withAPIKeyClaims injects API-key claims into the request context.
func withAPIKeyClaims(r *http.Request, userID string) *http.Request {
	claims := &core.AuthClaims{UserID: userID, IsAPIKey: true}
	ctx := context.WithValue(r.Context(), core.ClaimsKey, claims)
	return r.WithContext(ctx)
}

// withJWTClaims injects JWT (non-api-key) claims into the request context.
func withJWTClaims(r *http.Request, userID string) *http.Request {
	claims := &core.AuthClaims{UserID: userID, IsAPIKey: false}
	ctx := context.WithValue(r.Context(), core.ClaimsKey, claims)
	return r.WithContext(ctx)
}

func TestAPIKeyAudit_RecordsAuditEntry(t *testing.T) {
	c := &auditCapture{}
	var handlerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-1")
	req.RemoteAddr = "192.0.2.1:12345"

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.True(t, handlerCalled, "next handler must be called")
	recs := c.records()
	assert.Len(t, recs, 1)
	assert.Equal(t, "key-1", recs[0].apiKeyID)
	assert.Equal(t, http.MethodGet, recs[0].method)
	assert.Equal(t, "/api/test", recs[0].path)
	assert.Equal(t, http.StatusOK, recs[0].status)
	assert.Equal(t, "192.0.2.1", recs[0].ip) // RemoteAddr when no trusted proxies
}

func TestAPIKeyAudit_JWTRequestsBypass(t *testing.T) {
	c := &auditCapture{}
	var handlerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	mw := middleware.APIKeyAudit(c.logFn(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withJWTClaims(req, "user-1")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.True(t, handlerCalled, "next handler must be called")
	assert.Empty(t, c.records(), "JWT requests must not be audited")
}

func TestAPIKeyAudit_NilLogFn_Passthrough(t *testing.T) {
	var handlerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	mw := middleware.APIKeyAudit(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-1")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.True(t, handlerCalled, "next handler must be called")
}

func TestAPIKeyAudit_CapturesErrorStatus(t *testing.T) {
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	mw := middleware.APIKeyAudit(c.logFn(), nil)
	req := httptest.NewRequest(http.MethodPost, "/api/forbidden", nil)
	req = withAPIKeyClaims(req, "key-2")
	req.RemoteAddr = "203.0.113.42:8080"

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	assert.Equal(t, http.StatusForbidden, recs[0].status)
}

func TestAPIKeyAudit_TrustedProxyNoXFF_UsesRemoteAddr(t *testing.T) {
	// Without XFF, RemoteAddr is used regardless of trusted proxies.
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), []*net.IPNet{trusted10})
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-3")
	req.RemoteAddr = "203.0.113.99:4567"

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	assert.Equal(t, "203.0.113.99", recs[0].ip)
}

func TestAPIKeyAudit_XFFSpoofedWithoutTrustedProxies(t *testing.T) {
	// Without trustedCIDRs, XFF is never trusted.
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-4")
	req.RemoteAddr = "203.0.113.1:12345"
	// Attacker spoofs XFF to claim they're from an internal IP.
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	// XFF is ignored: RemoteAddr is the truth.
	assert.Equal(t, "203.0.113.1", recs[0].ip)
}

func TestAPIKeyAudit_XFFHonoredWithTrustedProxy(t *testing.T) {
	// When the rightmost XFF IP is trusted, the first untrusted IP from
	// the right is the real client IP.
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), []*net.IPNet{trusted10})
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-5")
	req.RemoteAddr = "10.0.0.1:8080"
	// Client -> untrusted proxy 192.168.1.1 -> trusted proxy 10.0.0.1
	// With only 10.0.0.0/8 trusted, the real client IP is 192.168.1.1
	req.Header.Set("X-Forwarded-For", "192.168.1.1, 10.0.0.1")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	assert.Equal(t, "192.168.1.1", recs[0].ip)
}

func TestAPIKeyAudit_XFFSpoofedWithTrustedButUntrustedImmediateProxy(t *testing.T) {
	// ClientIPTrusted rejects the entire XFF chain when the rightmost IP
	// is not in trustedCIDRs, falling back to RemoteAddr.
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), []*net.IPNet{trusted10})
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-6")
	req.RemoteAddr = "203.0.113.50:12345"
	// Attacker claims: client=1.2.3.4, immediate proxy=203.0.113.50
	// But 203.0.113.50 is NOT in the trusted set -> XFF is rejected.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.50")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	// XFF rejected, falls back to RemoteAddr.
	assert.Equal(t, "203.0.113.50", recs[0].ip)
}

func TestAPIKeyAudit_MalformedXFF_FallsBackToRemoteAddr(t *testing.T) {
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	c := &auditCapture{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := middleware.APIKeyAudit(c.logFn(), []*net.IPNet{trusted10})
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = withAPIKeyClaims(req, "key-7")
	req.RemoteAddr = "10.0.0.1:8080"
	// Malformed IP in XFF should cause fallback.
	req.Header.Set("X-Forwarded-For", "not-an-ip")

	mw(next).ServeHTTP(httptest.NewRecorder(), req)

	recs := c.records()
	assert.Len(t, recs, 1)
	assert.Equal(t, "10.0.0.1", recs[0].ip)
}
