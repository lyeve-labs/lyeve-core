package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatencyTracker_RecordAndRetrieve(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)
	handler := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	// Fire 3 requests to the same endpoint
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	stats := tracker.Slowest(10)
	require.Len(t, stats, 1)
	assert.Equal(t, "GET", stats[0].Method)
	assert.Equal(t, "/api/v1/content/posts", stats[0].Path)
	assert.Equal(t, uint64(3), stats[0].Count)
	assert.Greater(t, stats[0].P95US, uint64(0))
	assert.Greater(t, stats[0].P99US, uint64(0))
	assert.Greater(t, stats[0].AvgUS, uint64(0))
}

func TestLatencyTracker_MultipleEndpoints(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)

	makeHandler := func() http.Handler {
		return tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	h := makeHandler()

	paths := []string{"/api/v1/content/posts", "/api/v1/content/users", "/api/v1/schemas"}
	for _, path := range paths {
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
		}
	}

	all := tracker.All()
	assert.Len(t, all, 3)

	// Each path should have 5 requests
	for _, s := range all {
		assert.Equal(t, uint64(5), s.Count)
	}
}

func TestLatencyTracker_Eviction(t *testing.T) {
	tracker := mw.NewLatencyTracker(2, 100)

	makeHandler := func() http.Handler {
		return tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	h := makeHandler()

	// Register 3 different endpoints with capacity 2: first should be evicted.
	for _, path := range []string{"/a", "/b", "/c"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	all := tracker.All()
	assert.Len(t, all, 2) // "/a" evicted

	// "/b" and "/c" should be present.
	paths := make(map[string]bool)
	for _, s := range all {
		paths[s.Path] = true
	}
	assert.True(t, paths["/b"])
	assert.True(t, paths["/c"])
	assert.False(t, paths["/a"])
}

func TestLatencyTracker_DifferentMethods(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)

	makeHandler := func() http.Handler {
		return tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	h := makeHandler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/content/posts", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	all := tracker.All()
	assert.Len(t, all, 2) // GET and POST are different endpoints

	methods := make(map[string]bool)
	for _, s := range all {
		methods[s.Method] = true
	}
	assert.True(t, methods["GET"])
	assert.True(t, methods["POST"])
}

func TestLatencyTracker_Stats(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)

	h := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	ts := tracker.Stats()
	assert.Equal(t, 1, ts.TrackedEndpoints)
	assert.Equal(t, 10, ts.MaxEndpoints)
	assert.Equal(t, 100, ts.SamplesPerEp)
}

func TestLatencyTracker_PercentileAccuracy(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 50)

	// Simulate known latencies by calling the handler with explicit timing.
	for _, dur := range []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
	} {
		h := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(dur)
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/perf", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	slowest := tracker.Slowest(1)
	require.Len(t, slowest, 1)
	stats := slowest[0]

	assert.Equal(t, uint64(5), stats.Count)

	// All should be >= 1000us (1ms) and p95 should be >= 5000us (5ms)
	assert.GreaterOrEqual(t, stats.MinUS, uint64(800))  // allow some jitter
	assert.GreaterOrEqual(t, stats.P95US, uint64(4000)) // p95 >= ~5ms
	assert.GreaterOrEqual(t, stats.P99US, uint64(8000)) // p99 >= ~10ms
}

func TestLatencyTracker_ZeroConfig(t *testing.T) {
	// Defaults should apply
	tracker := mw.NewLatencyTracker(0, 0)
	ts := tracker.Stats()
	assert.Equal(t, 200, ts.MaxEndpoints)
	assert.Equal(t, 500, ts.SamplesPerEp)
}

func TestLatencyTracker_ConcurrentSafety(t *testing.T) {
	tracker := mw.NewLatencyTracker(50, 100)

	h := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	paths := []string{"/a", "/b", "/c", "/d", "/e"}
	done := make(chan struct{})

	for _, path := range paths {
		go func(p string) {
			for i := 0; i < 20; i++ {
				req := httptest.NewRequest(http.MethodGet, p, nil)
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
			done <- struct{}{}
		}(path)
	}

	for range paths {
		<-done
	}

	all := tracker.All()
	assert.Len(t, all, 5)
	for _, s := range all {
		assert.Equal(t, uint64(20), s.Count)
	}
}

func TestLatencyTracker_JSONSerialization(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)

	h := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	slowest := tracker.Slowest(1)
	data, err := json.Marshal(slowest)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"method":"GET"`)
	assert.Contains(t, string(data), `"path":"/test"`)
	assert.Contains(t, string(data), `"p95_us"`)
}

func TestLatencyTracker_SlowestLimit(t *testing.T) {
	tracker := mw.NewLatencyTracker(10, 100)

	h := tracker.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/a", "/b", "/c", "/d", "/e"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Ask for 3 of 5
	slowest := tracker.Slowest(3)
	assert.Len(t, slowest, 3)

	// Ask for more than available
	slowest = tracker.Slowest(20)
	assert.Len(t, slowest, 5)
}
