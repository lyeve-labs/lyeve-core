package runtime

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// InflightDrainer tracks in-flight HTTP requests and provides a Wait
// primitive so the shutdown sequence can block until all active requests
// have completed (or the drain timeout fires).
//
// Usage:
//
//	drainer := NewInflightDrainer()
//	router.Use(drainer.Middleware)
//	// ... on shutdown:
//	drainer.WaitOr(30 * time.Second)
type InflightDrainer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	counter  int64
	acounter atomic.Int64 // active count for quick read via ActiveCount()
}

// NewInflightDrainer returns a ready-to-use drainer.
func NewInflightDrainer() *InflightDrainer {
	d := &InflightDrainer{}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// Middleware returns a chi-compatible middleware that increments the inflight
// counter on entry and decrements it when the request handler returns (after
// all deferred middleware unwinds). Placed early in the chain so it wraps
// the entire request lifecycle.
func (d *InflightDrainer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.counter++
		d.mu.Unlock()
		d.acounter.Add(1)
		defer func() {
			d.acounter.Add(-1)
			d.mu.Lock()
			d.counter--
			if d.counter == 0 {
				d.cond.Broadcast()
			}
			d.mu.Unlock()
		}()

		// Wrap ResponseWriter so we can inject 503 on late requests if needed.
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
	})
}

// WaitOr blocks until all in-flight requests complete or the timeout fires.
// Returns nil on clean drain, context.DeadlineExceeded on timeout.
func (d *InflightDrainer) WaitOr(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		d.mu.Lock()
		for d.counter > 0 {
			d.cond.Wait()
		}
		d.mu.Unlock()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ActiveCount returns the number of in-flight requests. Useful for logging
// during shutdown diagnostics.
func (d *InflightDrainer) ActiveCount() int64 {
	return d.acounter.Load()
}
