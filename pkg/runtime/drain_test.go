package runtime

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInflightDrainer_BasicCounting(t *testing.T) {
	d := NewInflightDrainer()

	assert.Equal(t, int64(0), d.ActiveCount())

	// Simulate 3 concurrent requests.
	var wg sync.WaitGroup
	started := make(chan struct{})
	barrier := make(chan struct{})

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-barrier
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			handler.ServeHTTP(rec, req)
		}()
	}

	// Wait for all 3 handlers to be inside the middleware.
	for i := 0; i < 3; i++ {
		<-started
	}

	assert.Equal(t, int64(3), d.ActiveCount())

	// Release all.
	close(barrier)
	wg.Wait()

	assert.Equal(t, int64(0), d.ActiveCount())
}

func TestInflightDrainer_WaitOr_CleanDrain(t *testing.T) {
	d := NewInflightDrainer()

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Fire a request that completes immediately.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fast", nil)
	handler.ServeHTTP(rec, req)

	// WaitOr should return nil since no requests are in-flight.
	err := d.WaitOr(1 * time.Second)
	require.NoError(t, err)
}

func TestInflightDrainer_WaitOr_Timeout(t *testing.T) {
	d := NewInflightDrainer()

	started := make(chan struct{})
	barrier := make(chan struct{})

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-barrier
		w.WriteHeader(http.StatusOK)
	}))

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		handler.ServeHTTP(rec, req)
	}()

	<-started
	assert.Equal(t, int64(1), d.ActiveCount())

	// WaitOr should time out since the request is still in-flight.
	start := time.Now()
	err := d.WaitOr(50 * time.Millisecond)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, elapsed < 500*time.Millisecond, "should have timed out quickly")

	// Clean up.
	close(barrier)
	// Let the handler finish.
	time.Sleep(10 * time.Millisecond)
}

func TestInflightDrainer_WaitOr_WaitsForCompletion(t *testing.T) {
	d := NewInflightDrainer()

	started := make(chan struct{})
	barrier := make(chan struct{})

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-barrier
		w.WriteHeader(http.StatusOK)
	}))

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/delayed", nil)
		handler.ServeHTTP(rec, req)
	}()

	<-started

	// Release the request after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(barrier)
	}()

	// WaitOr should succeed (request completes within the timeout).
	err := d.WaitOr(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.ActiveCount())
}

func TestInflightDrainer_Middleware_PanickedRequest(t *testing.T) {
	d := NewInflightDrainer()

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler panic")
	}))

	// The middleware defers should still decrement even if the handler panics.
	// However, chi's Recoverer middleware (which runs above) catches panics.
	// Without Recoverer, the panic will propagate and the defer will still run.
	require.Panics(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/panic", nil)
		handler.ServeHTTP(rec, req)
	})

	// Counter should be back to 0 since the deferred decrement ran.
	assert.Equal(t, int64(0), d.ActiveCount())
}

func TestInflightDrainer_ConcurrentSafety(t *testing.T) {
	d := NewInflightDrainer()

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/concurrent", nil)
			handler.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(0), d.ActiveCount())
}

func TestInflightDrainer_MixedCompletion_WaitOrSucceeds(t *testing.T) {
	d := NewInflightDrainer()

	var active atomic.Int64

	handler := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		// Some requests are fast, some are slow.
		if r.URL.Path == "/slow" {
			time.Sleep(20 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		path := "/fast"
		if i%3 == 0 {
			path = "/slow"
		}
		go func(p string) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, p, nil)
			handler.ServeHTTP(rec, req)
		}(path)
	}

	// WaitOr in parallel: should succeed after all requests finish.
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.WaitOr(5 * time.Second)
	}()

	wg.Wait()
	err := <-errCh
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.ActiveCount())
}
