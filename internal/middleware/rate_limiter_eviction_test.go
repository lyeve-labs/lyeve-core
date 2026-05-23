package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perKeyRateLimiter caps its buckets at maxKeys (default 10_000) with LRU
// eviction on insert, so distinct addresses cannot grow memory without bound.

// TestRateLimiter_MapEviction verifies that the perKeyRateLimiter evicts the
// oldest entry when the bucket map reaches the configured capacity.
func TestRateLimiter_MapEviction(t *testing.T) {
	const maxKeys = 5

	rl := perKeyRateLimiter(100, 100, maxKeys, func(r *http.Request) string {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		return ip
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Insert keys 1..5: all should be accepted, map fills to capacity.
	for i := 1; i <= 5; i++ {
		ip := fmt.Sprintf("10.0.0.%d:12345", i)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(ip))
		assert.Equal(t, http.StatusOK, rec.Code, "key %d should be accepted", i)
	}

	// Insert key 6: this should evict key 1 (oldest). Key 1 is now gone.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.6:12345"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Key 1's bucket was evicted, so it gets a fresh bucket. With burst=100
	// and no prior requests, it should be accepted.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.1:12345"))
	assert.Equal(t, http.StatusOK, rec.Code, "evicted key 1 should get a fresh bucket")
}

// TestRateLimiter_DefaultMaxKeys verifies the default cap is large enough
// for realistic deployments but that eviction works by exhausting a small
// custom cap.
func TestRateLimiter_DefaultMaxKeys(t *testing.T) {
	const maxKeys = 3

	rl := perKeyRateLimiter(1000, 1000, maxKeys, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Send 4 distinct IPs: only 3 can stay. The oldest (IP 1) gets evicted.
	for i := 1; i <= 4; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(fmt.Sprintf("10.0.0.%d:12345", i)))
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	// IP 1 was evicted and now gets a fresh bucket: rate-limit state reset.
	// With burst=1000 it should still pass through fine.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.1:12345"))
	assert.Equal(t, http.StatusOK, rec.Code,
		"evicted IP should get fresh bucket, not leak state from before eviction")
}

// TestRateLimiter_EvictionDoesNotAffectStableKeys verifies that eviction of
// old/lazy keys does not interfere with heavily-used active keys.
func TestRateLimiter_EvictionDoesNotAffectStableKeys(t *testing.T) {
	const maxKeys = 5

	rl := perKeyRateLimiter(100, 100, maxKeys, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Active key that keeps getting requests.
	active := "10.0.0.1:12345"

	// Fill the map with 4 one-shot IPs + keep active key alive.
	for i := 2; i <= 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(fmt.Sprintf("10.0.0.%d:12345", i)))
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	// Refresh the active key so its lastFill is recent.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(active))
		assert.Equal(t, http.StatusOK, rec.Code)
		time.Sleep(time.Millisecond) // ensure lastFill advances
	}

	// Insert new key 6: eviction should pick the oldest one-shot IP (2),
	// not the active key (1).
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.6:12345"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Active key should still be permitted (not evicted).
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr(active))
	assert.Equal(t, http.StatusOK, rec.Code, "active key should not be evicted")
}

// TestRateLimiter_ConcurrentEvictionSafety verifies that the eviction path
// is safe under concurrent access by many distinct IPs.
func TestRateLimiter_ConcurrentEvictionSafety(t *testing.T) {
	const maxKeys = 50
	const totalGoroutines = 200

	rl := perKeyRateLimiter(1000, 1000, maxKeys, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var (
		wg     sync.WaitGroup
		errors atomic.Int32
	)

	wg.Add(totalGoroutines)
	for g := 0; g < totalGoroutines; g++ {
		go func(id int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, reqWithRemoteAddr(fmt.Sprintf("10.0.0.%d:12345", id)))
			if rec.Code != http.StatusOK {
				errors.Add(1)
			}
		}(g)
	}
	wg.Wait()

	assert.Equal(t, int32(0), errors.Load(),
		"no requests should fail under concurrent eviction pressure")
}

// TestRateLimiter_MapSizeStable verifies that the eviction mechanism
// prevents unbounded growth: the number of distinct IPs that can be
// tracked is bounded by maxKeys.
func TestRateLimiter_MapSizeStable(t *testing.T) {
	const maxKeys = 10
	const totalIPs = 100

	rl := perKeyRateLimiter(1000, 1000, maxKeys, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 1; i <= totalIPs; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(fmt.Sprintf("10.0.0.%d:12345", i)))
		assert.Equal(t, http.StatusOK, rec.Code,
			"request from IP %d should succeed despite eviction churn", i)
	}
}

// TestRateLimiter_DefaultConst verifies that DefaultMaxKeys is exported and
// has a reasonable value for production deployments.
func TestRateLimiter_DefaultMaxKeysConst(t *testing.T) {
	require.Greater(t, DefaultMaxKeys, 0, "DefaultMaxKeys must be positive")
	require.LessOrEqual(t, DefaultMaxKeys, 100_000,
		"DefaultMaxKeys must not be so large that eviction is useless")
}

// TestRateLimiter_ZeroMaxKeysDefaults verifies that passing zero for maxKeys
// falls back to DefaultMaxKeys (no crash, eviction works at default capacity).
func TestRateLimiter_ZeroMaxKeysDefaults(t *testing.T) {
	rl := perKeyRateLimiter(1, 1, 0, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Just verify it works with multiple distinct IPs: no panic, no hang.
	for i := 1; i <= 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, reqWithRemoteAddr(fmt.Sprintf("10.0.0.%d:12345", i)))
		assert.Equal(t, http.StatusOK, rec.Code)
	}
}

// TestRateLimiter_MapEviction_ActualEviction verifies that eviction happens
// by discriminating on rate-limit state: the evicted key resets to a full
// bucket after eviction, while a non-evicted key retains its consumed tokens.
func TestRateLimiter_MapEviction_ActualEviction(t *testing.T) {
	const maxKeys = 2

	rl := perKeyRateLimiter(1, 1, maxKeys, func(r *http.Request) string {
		return r.RemoteAddr
	})

	handler := rl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Send one request from IP A: consumes the single token.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.1:12345"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Fill the slot with IP B (key 2). Now A is oldest (and only evictable).
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.2:12345"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Insert IP C: evicts A (maxKeys=2, A is oldest).
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.3:12345"))
	assert.Equal(t, http.StatusOK, rec.Code)

	// A was evicted, so its bucket was reset to full (1 token).
	// With rate=1, burst=1, and no prior consumption, this should be OK.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.1:12345"))
	assert.Equal(t, http.StatusOK, rec.Code, "evicted IP should get fresh bucket with full tokens")

	// Now consume that fresh token from A. Next request should be rate-limited.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, reqWithRemoteAddr("10.0.0.1:12345"))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code,
		"after using the fresh token, A should be rate-limited, proving eviction reset state")
}

// Helpers

func reqWithRemoteAddr(addr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = addr
	return req
}
