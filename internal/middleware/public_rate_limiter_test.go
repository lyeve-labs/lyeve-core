package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PublicEndpointRateLimiter

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func TestPublicEndpointRateLimiter_BasicEnforcement(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/passwordless/request": {Rate: 10, Burst: 3},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))

	// First 3 requests (burst) should pass.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should pass", i)
	}

	// 4th request should be rate limited.
	req := httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, "3", rec.Header().Get("RateLimit-Limit"))
}

func TestPublicEndpointRateLimiter_PerIPIsolation(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"GET:/api/admin/auth/oauth-providers": {Rate: 10, Burst: 2},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("GET", "/api/admin/auth/oauth-providers", http.HandlerFunc(okHandler))

	// Exhaust IP1.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/oauth-providers", nil)
		req.RemoteAddr = "10.0.0.1:1111"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	// IP1 exhausted.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/oauth-providers", nil)
	req.RemoteAddr = "10.0.0.1:1111"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// IP2 should still pass.
	req = httptest.NewRequest(http.MethodGet, "/api/admin/auth/oauth-providers", nil)
	req.RemoteAddr = "10.0.0.2:2222"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestPublicEndpointRateLimiter_PerEndpointIsolation(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/passwordless/request": {Rate: 10, Burst: 1},
		"POST:/auth/passwordless/verify":  {Rate: 10, Burst: 1},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	reqHandler := limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))
	verifyHandler := limiter.Wrap("POST", "/auth/passwordless/verify", http.HandlerFunc(okHandler))

	ip := "10.0.0.5:9999"

	// Exhaust request endpoint.
	r := httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	reqHandler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)

	r = httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	r.RemoteAddr = ip
	w = httptest.NewRecorder()
	reqHandler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// Verify endpoint still works.
	r = httptest.NewRequest(http.MethodPost, "/auth/passwordless/verify", nil)
	r.RemoteAddr = ip
	w = httptest.NewRecorder()
	verifyHandler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPublicEndpointRateLimiter_GlobalCap(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"GET:/endpoint-a": {Rate: 100, Burst: 100},
		"GET:/endpoint-b": {Rate: 100, Burst: 100},
	}
	global := &PublicRateLimitConfig{Rate: 10, Burst: 2}
	limiter := NewPublicEndpointRateLimiter(configs, global, nil)
	handlerA := limiter.Wrap("GET", "/endpoint-a", http.HandlerFunc(okHandler))
	handlerB := limiter.Wrap("GET", "/endpoint-b", http.HandlerFunc(okHandler))

	ip := "10.0.0.1:1234"

	// 2 requests to A (within global burst).
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/endpoint-a", nil)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		handlerA.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code, "A request %d", i)
	}

	// 3rd request to B should hit global cap.
	r := httptest.NewRequest(http.MethodGet, "/endpoint-b", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	handlerB.ServeHTTP(w, r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "global cap should block cross-endpoint")
}

func TestPublicEndpointRateLimiter_NoConfigPassThrough(t *testing.T) {
	// No config for this endpoint: should pass through.
	configs := map[string]PublicRateLimitConfig{}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("GET", "/unknown", http.HandlerFunc(okHandler))

	r := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
}

// Loopback is not exempt: with the proxy and the engine sharing a host, every
// request arrives from 127.0.0.1, so exempting it would switch brute-force
// protection off on a single-box deployment.
func TestPublicEndpointRateLimiter_LoopbackIsLimited(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:5000", "[::1]:5000"} {
		t.Run(addr, func(t *testing.T) {
			configs := map[string]PublicRateLimitConfig{
				"POST:/auth/passwordless/request": {Rate: 10, Burst: 1},
			}
			limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
			handler := limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))

			limited := false
			for i := 0; i < 10; i++ {
				r := httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
				r.RemoteAddr = addr
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code == http.StatusTooManyRequests {
					limited = true
					break
				}
			}
			assert.True(t, limited, "a burst from %s must be rate limited", addr)
		})
	}
}

func TestPublicEndpointRateLimiter_HealthBypass(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"GET:/health": {Rate: 10, Burst: 1},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("GET", "/health", http.HandlerFunc(okHandler))

	// Health endpoints bypass rate limiting.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.RemoteAddr = "10.0.0.1:5000"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code, "health request %d", i)
	}
}

func TestPublicEndpointRateLimiter_Refill(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/passwordless/request": {Rate: 1000, Burst: 1},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))

	ip := "10.0.0.1:1234"
	r := httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)

	// Exhausted.
	r = httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	r.RemoteAddr = ip
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// Wait for refill (1000 rps, 1 token needed -> 2ms should be enough).
	time.Sleep(3 * time.Millisecond)

	r = httptest.NewRequest(http.MethodPost, "/auth/passwordless/request", nil)
	r.RemoteAddr = ip
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code, "should refill after wait")
}

func TestPublicEndpointRateLimiter_Headers(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"GET:/test": {Rate: 10, Burst: 5},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("GET", "/test", http.HandlerFunc(okHandler))

	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "5", w.Header().Get("RateLimit-Limit"))
	assert.NotEmpty(t, w.Header().Get("RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("RateLimit-Reset"))
	assert.Equal(t, "5", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "5;w=1", w.Header().Get("RateLimit-Policy"))
}

func TestPublicEndpointRateLimiter_429Response(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/test": {Rate: 10, Burst: 1},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("POST", "/test", http.HandlerFunc(okHandler))

	ip := "10.0.0.1:1234"

	// Use up burst.
	r := httptest.NewRequest(http.MethodPost, "/test", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)

	// 429.
	r = httptest.NewRequest(http.MethodPost, "/test", nil)
	r.RemoteAddr = ip
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "rate limit exceeded")
	assert.Contains(t, w.Body.String(), `"limit":1`)
}

func TestPublicEndpointRateLimiter_Concurrent(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"GET:/test": {Rate: 0.001, Burst: 1000}, // near-zero rate: no refill during test
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)
	handler := limiter.Wrap("GET", "/test", http.HandlerFunc(okHandler))

	var allowed int32
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				r := httptest.NewRequest(http.MethodGet, "/test", nil)
				r.RemoteAddr = "10.0.0.1:1234"
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code == http.StatusOK {
					atomic.AddInt32(&allowed, 1)
				}
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	// Should have allowed exactly burst (1000): the rest should be 429.
	// Allow small tolerance for floating-point timing in concurrent access.
	assert.InDelta(t, int32(1000), atomic.LoadInt32(&allowed), 20, "should allow approximately burst requests")
}

// DefaultPublicRateLimits

// The table holds a row for each public route the engine serves itself and
// none for a route another owner serves, which declares its own. No row lets
// a single address burst past the global cap.
func TestDefaultPublicRateLimits_Coverage(t *testing.T) {
	configs, global := DefaultPublicRateLimits()
	require.NotNil(t, global)

	engineRoutes := []string{
		"POST:/api/admin/auth/login",
		"POST:/api/admin/auth/refresh",
		"POST:/api/admin/auth/mfa-verify",
		"POST:/api/admin/auth/device",
		"POST:/api/admin/auth/device/token",
		"GET:/api/admin/auth/device/{user_code}",
		"POST:/api/admin/auth/device/{user_code}/approve",
		"POST:/api/admin/auth/device/{user_code}/deny",
		"POST:/api/admin/gdpr/export",
		"POST:/api/admin/gdpr/erase",
		"POST:/api/v1/auth/token",
	}
	keys := make([]string, 0, len(configs))
	for key := range configs {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, engineRoutes, keys)

	assert.Greater(t, global.Burst, 0)
	for _, cfg := range configs {
		assert.LessOrEqual(t, cfg.Burst, global.Burst,
			"individual burst should not exceed global burst")
	}
}

// Bucket growth is bounded

func TestPublicEndpointRateLimiter_MaxIPsEviction(t *testing.T) {
	// Create a limiter with a tiny cap (3 IPs) and no global cap so we
	// only add per-endpoint IP buckets.
	cfg := PublicEndpointRateLimiterConfig{
		Configs: map[string]PublicRateLimitConfig{
			"GET:/test": {Rate: 1000, Burst: 1},
		},
		MaxIPs: 3,
	}
	limiter := newPublicEndpointRateLimiterWithConfig(cfg)
	handler := limiter.Wrap("GET", "/test", http.HandlerFunc(okHandler))

	// Use 3 distinct IPs: each creates a bucket.
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", i+1)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code, "IP %d", i+1)
	}
	assert.Equal(t, 3, limiter.buckets.len(), "should have exactly 3 buckets")

	// 4th IP should evict the oldest (10.0.0.1) and add its own.
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.RemoteAddr = "10.0.0.99:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code, "4th IP")

	// Still capped at 3.
	assert.Equal(t, 3, limiter.buckets.len(), "should still have 3 buckets after eviction")
	// 10.0.0.1 should have been evicted.
	hasOld := limiter.buckets.has("GET:/test:10.0.0.1")
	assert.False(t, hasOld, "oldest IP 10.0.0.1 should have been evicted")
	// 10.0.0.99 should be present.
	hasNew := limiter.buckets.has("GET:/test:10.0.0.99")
	assert.True(t, hasNew, "new IP 10.0.0.99 should be present")
}

func TestPublicEndpointRateLimiter_CleanupAndStop(t *testing.T) {
	// The cleanup interval must be comfortably longer than the 150-bucket
	// setup loop below. Under -race with coverage instrumentation that loop
	// takes tens of ms. A too-short interval lets the cleanup goroutine evict
	// the oldest buckets mid-setup, so the "expected 150" count flakes.
	cfg := PublicEndpointRateLimiterConfig{
		Configs: map[string]PublicRateLimitConfig{
			"GET:/test": {Rate: 1000, Burst: 100},
		},
		MaxIPs:          200,
		CleanupInterval: 200 * time.Millisecond,
	}
	limiter := newPublicEndpointRateLimiterWithConfig(cfg)
	defer limiter.Stop()

	handler := limiter.Wrap("GET", "/test", http.HandlerFunc(okHandler))

	// Create 150 buckets (above the 100-bucket cleanup floor) so the
	// cleanup goroutine's `len(buckets) > 100` guard passes.
	for i := 0; i < 150; i++ {
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", i+1)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code, "IP %d", i+1)
	}
	assert.Equal(t, 150, limiter.buckets.len(), "should have 150 buckets")

	// All buckets are now stale (lastFill from creation time).
	// Poll until cleanup ticks evict them down to the floor.
	deadline := time.Now().Add(3 * time.Second)
	for {
		count := 0
		func() {
			limiter.mu.Lock()
			defer limiter.mu.Unlock()
			count = limiter.buckets.len()
		}()
		// Cleanup never empties below 100 (thundering-herd guard).
		if count <= 100 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	func() {
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		assert.LessOrEqual(t, limiter.buckets.len(), 100,
			"stale buckets should have been cleaned down to the 100-bucket floor")
	}()

	// Stop should be idempotent.
	limiter.Stop()
	limiter.Stop()
}

func TestPublicEndpointRateLimiter_WithConfigDefaults(t *testing.T) {
	// Creating with empty config should apply MaxIPs default (10_000)
	// and no cleanup goroutine.
	cfg := PublicEndpointRateLimiterConfig{
		Configs: map[string]PublicRateLimitConfig{
			"GET:/test": {Rate: 10, Burst: 5},
		},
	}
	limiter := newPublicEndpointRateLimiterWithConfig(cfg)
	assert.Equal(t, 10_000, limiter.maxIPs, "default MaxIPs should be 10_000")
	assert.Nil(t, limiter.stopCleanup, "no cleanup goroutine when CleanupInterval is zero")

	// Stop is a no-op when there's no goroutine.
	limiter.Stop()

	// Bucket creation still works.
	handler := limiter.Wrap("GET", "/test", http.HandlerFunc(okHandler))
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
}

// Verify: startup assertion for wrapped-route config coverage

func TestVerify_AllConfigured(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/login":    {Rate: 5, Burst: 10},
		"POST:/auth/refresh":  {Rate: 5, Burst: 10},
		"GET:/auth/providers": {Rate: 30, Burst: 50},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)

	// Wrap the same routes that have configs.
	limiter.Wrap("POST", "/auth/login", http.HandlerFunc(okHandler))
	limiter.Wrap("POST", "/auth/refresh", http.HandlerFunc(okHandler))
	limiter.WrapFunc("GET", "/auth/providers")(http.HandlerFunc(okHandler))

	err := limiter.Verify()
	assert.NoError(t, err)
}

func TestVerify_MissingConfig_NoGlobal(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/login": {Rate: 5, Burst: 10},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)

	// Wrap a route that has a config...
	limiter.Wrap("POST", "/auth/login", http.HandlerFunc(okHandler))
	// ...and one that doesn't.
	limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))

	err := limiter.Verify()
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "no per-endpoint config")
	assert.Contains(t, errStr, "no global cap")
	assert.Contains(t, errStr, "POST:/auth/passwordless/request")
	// The configured route should NOT be in the error.
	assert.NotContains(t, errStr, "POST:/auth/login")
}

func TestVerify_MissingConfig_WithGlobal(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/login": {Rate: 5, Burst: 10},
	}
	global := &PublicRateLimitConfig{Rate: 50, Burst: 100}
	limiter := NewPublicEndpointRateLimiter(configs, global, nil)

	limiter.Wrap("POST", "/auth/login", http.HandlerFunc(okHandler))
	limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))

	err := limiter.Verify()
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "global cap present but no per-endpoint config")
	assert.Contains(t, errStr, "POST:/auth/passwordless/request")
}

func TestVerify_NoWrappedRoutes(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/login": {Rate: 5, Burst: 10},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)

	// Nothing wrapped: Verify should succeed.
	err := limiter.Verify()
	assert.NoError(t, err)
}

func TestVerify_MultipleMissing(t *testing.T) {
	configs := map[string]PublicRateLimitConfig{
		"POST:/auth/login": {Rate: 5, Burst: 10},
	}
	limiter := NewPublicEndpointRateLimiter(configs, nil, nil)

	limiter.Wrap("POST", "/auth/login", http.HandlerFunc(okHandler))
	limiter.Wrap("POST", "/auth/passwordless/request", http.HandlerFunc(okHandler))
	limiter.WrapFunc("GET", "/auth/saml-providers")(http.HandlerFunc(okHandler))

	err := limiter.Verify()
	require.Error(t, err)

	errStr := err.Error()
	assert.Contains(t, errStr, "2 wrapped route(s)")
	assert.Contains(t, errStr, "POST:/auth/passwordless/request")
	assert.Contains(t, errStr, "GET:/auth/saml-providers")
}
