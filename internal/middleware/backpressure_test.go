package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bpOKHandler returns an http.Handler that writes 200 OK.
// Named to avoid collision with the package-level okHandler in other test files.
func bpOKHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	})
}

func slowHandler(d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(d)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	})
}

func doReq(t *testing.T, mw func(http.Handler) http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	mw(bpOKHandler()).ServeHTTP(rec, req)
	return rec
}

func doReqWithTenantHeader(t *testing.T, mw func(http.Handler) http.Handler, path, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tenantID != "" {
		// Inject tenant directly into context via the unexported key
		// (we're in package middleware so this is legal).
		ctx := context.WithValue(req.Context(), tenantKey{}, tenantID)
		req = req.WithContext(ctx)
	}
	mw(bpOKHandler()).ServeHTTP(rec, req)
	return rec
}

// Tests

func TestBackpressure_DefaultConfig(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{})
	require.NoError(t, err)
	defer bp.Stop()

	rec := doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBackpressure_HealthBypass(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{MaxInflight: 1})
	require.NoError(t, err)
	defer bp.Stop()

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/slow", nil)
		bp.Middleware(slowHandler(2*time.Second)).ServeHTTP(rec, req)
	}()
	time.Sleep(10 * time.Millisecond)

	for _, path := range []string{"/healthz", "/readyz", "/startup", "/health", "/ready", "/metrics"} {
		rec := doReq(t, bp.Middleware, path)
		assert.Equal(t, http.StatusOK, rec.Code, "path %s should bypass backpressure", path)
	}
}

func TestBackpressure_GlobalConcurrencyCap(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           3,
		TenantQuotaPct:        1.0, // each tenant gets ceil(3*1.0)=3 slots
		RetryAfterSeconds:     2,
		PoolPressureThreshold: 1.0, // disable pool pressure
	})
	require.NoError(t, err)
	defer bp.Stop()

	var wg sync.WaitGroup
	started := make(chan struct{}, 3)
	block := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-block
		w.WriteHeader(http.StatusOK)
	})

	// Saturate the global semaphore from different tenants to avoid
	// hitting any single tenant's quota.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
			ctx := context.WithValue(req.Context(), tenantKey{}, fmt.Sprintf("tenant-%d", n))
			req = req.WithContext(ctx)
			bp.Middleware(handler).ServeHTTP(rec, req)
		}(i)
	}

	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for in-flight requests")
		}
	}

	// 4th request from another tenant: shed by global cap, not tenant quota.
	rec := doReqWithTenantHeader(t, bp.Middleware, "/api/v1/content", "tenant-new")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
	assert.Equal(t, "global_concurrency", rec.Header().Get("X-Shedding-Reason"))
	assert.Contains(t, rec.Body.String(), "server at capacity")

	close(block)
	wg.Wait()

	rec = doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBackpressure_GlobalConcurrency_Stats(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           5,
		TenantQuotaPct:        1.0,
		PoolPressureThreshold: 1.0,
	})
	require.NoError(t, err)
	defer bp.Stop()

	block := make(chan struct{})
	started := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-block
		w.WriteHeader(http.StatusOK)
	})

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
		bp.Middleware(handler).ServeHTTP(rec, req)
	}()

	<-started

	stats := bp.Stats()
	assert.Equal(t, int64(1), stats.GlobalInflight)
	assert.Equal(t, int64(5), stats.GlobalMax)
	assert.False(t, stats.Shedding)

	close(block)
	time.Sleep(10 * time.Millisecond)
}

func TestBackpressure_PoolPressure(t *testing.T) {
	var stats atomic.Value
	stats.Store(PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    5,
		InUse:              3,
	})

	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           100,
		TenantQuotaPct:        1.0,
		PoolStats:             func() PoolStats { return stats.Load().(PoolStats) },
		PoolPressureThreshold: 0.8,
		PoolPollInterval:      50 * time.Millisecond,
	})
	require.NoError(t, err)
	defer bp.Stop()

	time.Sleep(100 * time.Millisecond) // let poller run
	rec := doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusOK, rec.Code)

	stats.Store(PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    9,
		InUse:              9,
	})
	time.Sleep(200 * time.Millisecond)

	rec = doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "pool_pressure", rec.Header().Get("X-Shedding-Reason"))
	assert.Contains(t, rec.Body.String(), "database pool pressure")
}

func TestBackpressure_PoolPressureRecovery(t *testing.T) {
	var stats atomic.Value
	stats.Store(PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    9,
		InUse:              9,
	})

	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           100,
		TenantQuotaPct:        1.0,
		PoolStats:             func() PoolStats { return stats.Load().(PoolStats) },
		PoolPressureThreshold: 0.8,
		PoolPollInterval:      50 * time.Millisecond,
	})
	require.NoError(t, err)
	defer bp.Stop()

	time.Sleep(150 * time.Millisecond)
	rec := doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	stats.Store(PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    5,
		InUse:              2,
	})
	time.Sleep(200 * time.Millisecond)

	rec = doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBackpressure_PerTenantFairShare(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           10,
		TenantQuotaPct:        0.2, // each tenant gets 2 slots (10 * 0.2)
		PoolPressureThreshold: 1.0, // disable
	})
	require.NoError(t, err)
	defer bp.Stop()

	var wg sync.WaitGroup
	started := make(chan struct{}, 2)
	block := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-block
		w.WriteHeader(http.StatusOK)
	})

	// Saturate tenant-A's 2-slot quota.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
			ctx := context.WithValue(req.Context(), tenantKey{}, "tenant-a")
			req = req.WithContext(ctx)
			bp.Middleware(handler).ServeHTTP(rec, req)
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for tenant-a in-flight requests")
		}
	}

	// tenant-A's 3rd request is shed.
	rec := doReqWithTenantHeader(t, bp.Middleware, "/api/v1/content", "tenant-a")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "tenant_quota", rec.Header().Get("X-Shedding-Reason"))

	// tenant-B still has capacity.
	rec = doReqWithTenantHeader(t, bp.Middleware, "/api/v1/content", "tenant-b")
	assert.Equal(t, http.StatusOK, rec.Code)

	close(block)
	wg.Wait()
}

func TestBackpressure_AnonymousTenant(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           10,
		TenantQuotaPct:        0.3, // 3 slots per tenant
		PoolPressureThreshold: 1.0,
	})
	require.NoError(t, err)
	defer bp.Stop()

	// Untenanted requests share the _anonymous bucket.
	rec := doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBackpressure_CustomBypassPaths(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           1,
		TenantQuotaPct:        1.0,
		PoolPressureThreshold: 1.0,
		BypassPaths:           []string{"/custom-health", "/internal"},
	})
	require.NoError(t, err)
	defer bp.Stop()

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/slow", nil)
		bp.Middleware(slowHandler(2*time.Second)).ServeHTTP(rec, req)
	}()
	time.Sleep(10 * time.Millisecond)

	rec := doReq(t, bp.Middleware, "/custom-health")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = doReq(t, bp.Middleware, "/internal/debug")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = doReq(t, bp.Middleware, "/api/v1/content")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestBackpressure_StopIsIdempotent(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{})
	require.NoError(t, err)
	bp.Stop()
	bp.Stop() // should not panic
}

func TestBackpressure_StatsEmpty(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           50,
		TenantQuotaPct:        1.0,
		PoolPressureThreshold: 1.0,
	})
	require.NoError(t, err)
	defer func() { _ = bp.Stats }() // just ensure it doesn't panic

	stats := bp.Stats()
	assert.Equal(t, int64(0), stats.GlobalInflight)
	assert.Equal(t, int64(50), stats.GlobalMax)
	assert.Equal(t, 0.0, stats.PoolPressure)
	assert.False(t, stats.Shedding)
}

func TestBackpressure_ConcurrentAccess(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           50,
		TenantQuotaPct:        0.5,
		PoolPressureThreshold: 1.0,
	})
	require.NoError(t, err)
	defer bp.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tid := fmt.Sprintf("tenant-%d", n%5)
			rec := doReqWithTenantHeader(t, bp.Middleware, "/api/v1/content", tid)
			assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, rec.Code)
		}(i)
	}
	wg.Wait()
}

func TestBackpressure_StatsDuringLoad(t *testing.T) {
	bp, err := Backpressure(BackpressureConfig{
		MaxInflight:           10,
		TenantQuotaPct:        1.0,
		PoolPressureThreshold: 1.0,
	})
	require.NoError(t, err)
	defer func() { _ = bp.Stats }()

	var wg sync.WaitGroup
	started := make(chan struct{}, 10)
	block := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
			bp.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				started <- struct{}{}
				<-block
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)
		}()
	}

	for i := 0; i < 10; i++ {
		<-started
	}

	stats := bp.Stats()
	assert.Equal(t, int64(10), stats.GlobalInflight)

	close(block)
	wg.Wait()
}

func TestIsBackpressureBypass(t *testing.T) {
	tests := []struct {
		path   string
		bypass map[string]struct{}
		want   bool
	}{
		{"/healthz", map[string]struct{}{"/healthz": {}}, true},
		{"/health", map[string]struct{}{"/healthz": {}}, false},
		{"/api/v1/content", map[string]struct{}{"/healthz": {}}, false},
		{"/metrics", map[string]struct{}{"/healthz": {}, "/metrics": {}}, true},
		{"", map[string]struct{}{"/health": {}}, false},
		{"/healthz/extra", map[string]struct{}{"/healthz": {}}, true},
	}

	for _, tt := range tests {
		got := isBackpressureBypass(tt.path, tt.bypass)
		assert.Equal(t, tt.want, got, "path=%q", tt.path)
	}
}

// TestBackpressure_DoesNotShedJWKS pins the bypass for the public key
// endpoint. JWKS serves a cached public key and touches no database, so
// shedding it turns a load spike into an authentication outage across every
// service that trusts this one.
func TestBackpressure_DoesNotShedJWKS(t *testing.T) {
	for _, path := range []string{
		"/.well-known/jwks.json",
		"/.well-known/openid-configuration",
		"/healthz",
		"/readyz",
	} {
		if !isBackpressureBypass(path, bypassSetFrom(defaultBackpressureBypass)) {
			t.Errorf("%s is subject to backpressure; it must be exempt", path)
		}
	}
	// The exemption is for well-known documents, not for the API at large.
	for _, path := range []string{"/api/v1/content/post", "/api/admin/users"} {
		if isBackpressureBypass(path, bypassSetFrom(defaultBackpressureBypass)) {
			t.Errorf("%s is exempt from backpressure; shedding must still reach it", path)
		}
	}
}

func bypassSetFrom(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}
