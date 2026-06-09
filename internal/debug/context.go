package debug

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

type ctxKey struct{}

// TracerFromCtx retrieves the debug tracer from context, if any.
// Returns nil when debug mode is not active for this request.
func TracerFromCtx(ctx context.Context) *Tracer {
	t, _ := ctx.Value(ctxKey{}).(*Tracer)
	return t
}

// WithTracer returns a new context with the tracer attached.
func WithTracer(ctx context.Context, t *Tracer) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// IsDebugActive returns true when a debug tracer is present in the context.
func IsDebugActive(ctx context.Context) bool {
	return TracerFromCtx(ctx) != nil
}

// TimedMiddleware returns a middleware that times the execution of an inner
// handler and records it as a middleware segment in the debug tracer.
// When no tracer is in context, acts as a pass-through.
//
// Usage:
//
//	r.Use(debug.TimedMiddleware("rate-limiter"))
func TimedMiddleware(name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := TracerFromCtx(r.Context())
			if t == nil {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			next.ServeHTTP(w, r)
			t.Record(name, KindMiddleware, time.Since(start), nil)
		})
	}
}

// Response writer for response size tracking

// responseWriter tracks response size and status for an http.ResponseWriter.
type responseWriter struct {
	http.ResponseWriter
	mu     sync.Mutex
	size   int64
	status int
	wrote  bool
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.mu.Lock()
	if !rw.wrote {
		rw.status = code
		rw.wrote = true
	}
	rw.mu.Unlock()
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.mu.Lock()
	if !rw.wrote {
		rw.status = http.StatusOK
		rw.wrote = true
	}
	rw.mu.Unlock()
	n, err := rw.ResponseWriter.Write(b)
	rw.mu.Lock()
	rw.size += int64(n)
	rw.mu.Unlock()
	return n, err
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Size() int64 {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.size
}

func (rw *responseWriter) Status() int {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.status
}

// NewResponseWriter wraps an http.ResponseWriter to track response size and status.
func NewResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w}
}

// NewResponseWriterWithStatus wraps an http.ResponseWriter with a preset status, for
// callers that set the status before wrapping.
func NewResponseWriterWithStatus(w http.ResponseWriter, status int) *responseWriter {
	return &responseWriter{ResponseWriter: w, status: status, wrote: true}
}

// Hijack passes a connection upgrade through to the writer underneath.
// Without it a WebSocket or SSE handler mounted below this middleware cannot
// take the socket, and the handshake fails with the response already committed.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
