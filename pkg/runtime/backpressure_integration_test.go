// Integration tests for backpressure middleware wiring:
// adapter -> middleware -> chi router -> HTTP response.
// Pool pressure is simulated: no real database is needed.
package runtime

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// mockPoolStats is a thread-safe, togglable PoolStats source. The backpressure
// poller reads it from a background goroutine while the test mutates it, so
// access must be synchronized (the -race detector flags an unguarded var).
type mockPoolStats struct {
	mu    sync.Mutex
	stats mw.PoolStats
}

func (m *mockPoolStats) get() mw.PoolStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *mockPoolStats) set(s mw.PoolStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = s
}

func TestBackpressureWired_503UnderPoolPressure(t *testing.T) {
	// Simulate a saturated pool: 9/10 connections in use.
	poolFn := func() mw.PoolStats {
		return mw.PoolStats{
			MaxOpenConnections: 10,
			OpenConnections:    9,
			InUse:              9,
		}
	}

	bp, err := mw.Backpressure(mw.BackpressureConfig{
		MaxInflight:           100,
		TenantQuotaPct:        1.0, // disable per-tenant: only test pool pressure
		PoolStats:             poolFn,
		PoolPressureThreshold: 0.8, // 80%
		PoolPollInterval:      50 * time.Millisecond,
		RetryAfterSeconds:     3,
	})
	require.NoError(t, err)
	defer bp.Stop()

	// Wait for the background poller to pick up the saturated stats.
	time.Sleep(150 * time.Millisecond)

	r := chi.NewRouter()
	r.Use(bp.Middleware)
	r.Get("/api/v1/content", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Requests should be shed with 503 + Retry-After
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "3", rec.Header().Get("Retry-After"))
	assert.Equal(t, "pool_pressure", rec.Header().Get("X-Shedding-Reason"))
	assert.Contains(t, rec.Body.String(), "database pool pressure")

	// Health endpoint should still bypass
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestBackpressureWired_RecoversAfterPressureDrop(t *testing.T) {
	// Start saturated. The holder is thread-safe: the poller reads it from a
	// background goroutine while the test mutates it below.
	m := &mockPoolStats{stats: mw.PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    9,
		InUse:              9,
	}}

	bp, err := mw.Backpressure(mw.BackpressureConfig{
		MaxInflight:           100,
		TenantQuotaPct:        1.0,
		PoolStats:             m.get,
		PoolPressureThreshold: 0.8,
		PoolPollInterval:      50 * time.Millisecond,
		RetryAfterSeconds:     2,
	})
	require.NoError(t, err)
	defer bp.Stop()

	r := chi.NewRouter()
	r.Use(bp.Middleware)
	r.Get("/api/v1/content", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wait for poller to pick up saturation.
	time.Sleep(150 * time.Millisecond)

	// Confirm shedding.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Drop pressure below threshold.
	m.set(mw.PoolStats{
		MaxOpenConnections: 10,
		OpenConnections:    5,
		InUse:              2,
	})
	time.Sleep(150 * time.Millisecond)

	// Should recover.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	r.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestBackpressureWired_DisabledByDefault(t *testing.T) {
	// When PoolStats is nil, pool-pressure shedding is inactive.
	bp, err := mw.Backpressure(mw.BackpressureConfig{
		MaxInflight:           100,
		TenantQuotaPct:        1.0,
		PoolStats:             nil,
		PoolPressureThreshold: 0.8,
	})
	require.NoError(t, err)
	defer bp.Stop()

	r := chi.NewRouter()
	r.Use(bp.Middleware)
	r.Get("/api/v1/content", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, bp.Stats().Shedding)
}

func TestBackpressure_AdapterPoolStatsMapping(t *testing.T) {
	// Verify the poolStatsAdapter correctly maps database/sql.DBStats fields
	// into middleware.PoolStats.
	sqlStats := func() sql.DBStats {
		return sql.DBStats{
			MaxOpenConnections: 50,
			OpenConnections:    30,
			InUse:              25,
			WaitCount:          3,
			WaitDuration:       150 * time.Millisecond,
		}
	}
	adapter := poolStatsAdapter(sqlStats)
	result := adapter()

	assert.Equal(t, 50, result.MaxOpenConnections)
	assert.Equal(t, 30, result.OpenConnections)
	assert.Equal(t, 25, result.InUse)
	assert.Equal(t, int64(3), result.WaitCount)
	assert.Equal(t, 150*time.Millisecond, result.WaitDuration)
}
