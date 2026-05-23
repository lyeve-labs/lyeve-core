package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRateLimiter_ExactlyAtLimit(t *testing.T) {
	const burst = 4
	rl := RateLimiter(100, burst)
	h := rl(http.HandlerFunc(okHandler))
	ip := "10.0.0.1:1234"
	for i := 0; i < burst; i++ {
		mustStatus(t, h, ip, http.StatusOK, "req %d/%d", i+1, burst)
	}
	mustStatus(t, h, ip, http.StatusTooManyRequests, "burst+1 must block")
}

func TestRateLimiter_BurstWindowRollover(t *testing.T) {
	rl := RateLimiter(500, 1)
	h := rl(http.HandlerFunc(okHandler))
	ip := "10.0.0.2:1234"
	mustStatus(t, h, ip, http.StatusOK)
	mustStatus(t, h, ip, http.StatusTooManyRequests)
	time.Sleep(5 * time.Millisecond) // 500 rps -> 1 token refills in 2 ms
	mustStatus(t, h, ip, http.StatusOK, "refilled token")
	mustStatus(t, h, ip, http.StatusTooManyRequests, "token consumed, blocked again")
}

func TestPublicEndpointRateLimiter_PerEndpointVsGlobal(t *testing.T) {
	t.Run("per-endpoint stricter", func(t *testing.T) {
		configs := map[string]PublicRateLimitConfig{"GET:/a": {Rate: 100, Burst: 1}}
		global := &PublicRateLimitConfig{Rate: 100, Burst: 10}
		limiter := NewPublicEndpointRateLimiter(configs, global, nil)
		h := limiter.Wrap("GET", "/a", http.HandlerFunc(okHandler))
		mustStatus(t, h, "10.0.0.10:1", http.StatusOK)
		mustStatus(t, h, "10.0.0.10:1", http.StatusTooManyRequests, "per-endpoint cap")
	})
	t.Run("global stricter", func(t *testing.T) {
		configs := map[string]PublicRateLimitConfig{"GET:/b": {Rate: 100, Burst: 10}}
		global := &PublicRateLimitConfig{Rate: 100, Burst: 1}
		limiter := NewPublicEndpointRateLimiter(configs, global, nil)
		h := limiter.Wrap("GET", "/b", http.HandlerFunc(okHandler))
		mustStatus(t, h, "10.0.0.11:1", http.StatusOK)
		mustStatus(t, h, "10.0.0.11:1", http.StatusTooManyRequests, "global cap")
	})
}

func TestPerTenantRateLimiter_TenantIsolation(t *testing.T) {
	rl := PerTenantRateLimiter(100, 1)
	h := rl(http.HandlerFunc(okHandler))
	ip := "10.0.0.3:1234"
	ctxA := context.WithValue(context.Background(), tenantKey{}, "t-a")
	ctxB := context.WithValue(context.Background(), tenantKey{}, "t-b")

	assertOK := func(ctx context.Context, msgAndArgs ...any) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		r.RemoteAddr = ip
		h.ServeHTTP(rec, r)
		assert.Equal(t, http.StatusOK, rec.Code, msgAndArgs...)
	}
	assert429 := func(ctx context.Context, msgAndArgs ...any) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		r.RemoteAddr = ip
		h.ServeHTTP(rec, r)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, msgAndArgs...)
	}

	assertOK(ctxA)
	assert429(ctxA, "t-a exhausted")
	assertOK(ctxB, "t-b unaffected by t-a")
	// No-tenant fallback uses IP-only key: independent from tenant-keyed buckets.
	mustStatus(t, h, ip, http.StatusOK, "no-tenant ok despite t-a exhausted")
}

func TestRateLimiter_ZeroRate(t *testing.T) {
	rl := RateLimiter(0, 3)
	h := rl(http.HandlerFunc(okHandler))
	ip := "10.0.0.5:1234"
	for i := 0; i < 3; i++ {
		mustStatus(t, h, ip, http.StatusOK)
	}
	mustStatus(t, h, ip, http.StatusTooManyRequests)
	time.Sleep(50 * time.Millisecond)
	mustStatus(t, h, ip, http.StatusTooManyRequests, "zero rate never refills")
}

func TestRateLimiter_NegativeRate(t *testing.T) {
	rl := RateLimiter(-1, 2)
	h := rl(http.HandlerFunc(okHandler))
	ip := "10.0.0.6:1234"
	// Negative rate drains tokens as time passes. Hit until it blocks.
	blocked := false
	for i := 0; i < 5 && !blocked; i++ {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = ip
		h.ServeHTTP(rec, r)
		blocked = rec.Code == http.StatusTooManyRequests
	}
	assert.True(t, blocked, "negative rate should eventually block")
	time.Sleep(10 * time.Millisecond)
	mustStatus(t, h, ip, http.StatusTooManyRequests, "no refill with negative rate")
}

func mustStatus(t *testing.T, h http.Handler, ip string, want int, msgAndArgs ...any) {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = ip
	h.ServeHTTP(rec, r)
	assert.Equal(t, want, rec.Code, msgAndArgs...)
}
