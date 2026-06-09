package debug

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// AdminGate checks whether the request has admin-level auth (JWT with
// admin or super_admin role, or API key with admin scope).
type AdminGate func(r *http.Request) bool

// Handler returns HTTP middleware that activates debug mode when both
// X-Debug: true header is present AND the admin gate returns true.
//
// When debug is active:
//   - A Tracer is injected into the request context
//   - All subsequent handlers, DB queries, and hook events record timing
//   - The normal response body is replaced with a JSON debug Report
//   - Response headers from the downstream handler are captured in the report
//
// When debug is inactive (missing header or non-admin), the middleware
// is a no-op pass-through.
//
// Position: after JWT auth + requireAuth, ideally on admin-only routes.
// For non-admin callers, the header is silently ignored (no error).
func Handler(enabled bool, adminGate AdminGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled || r.Header.Get("X-Debug") != "true" || !adminGate(r) {
				next.ServeHTTP(w, r)
				return
			}

			t := NewTracer(r.Method, r.URL.Path)
			ctx := WithTracer(r.Context(), t)
			r = r.WithContext(ctx)

			// Capture the handler output in a buffer: the middleware replaces it
			// with the debug report.
			buf := &bytes.Buffer{}
			crw := &captureWriter{ResponseWriter: w, buf: buf, t: t, hdr: make(http.Header)}
			next.ServeHTTP(crw, r)

			// Record any response headers the handler set.
			for k, vv := range crw.hdr {
				for _, v := range vv {
					t.SetResponseHeader(k, v)
				}
			}

			report := t.Finalize(crw.status, crw.bytesWritten)

			data, err := report.MarshalIndent()
			if err != nil {
				http.Error(w, `{"error":"debug marshal failed"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Debug-Report-ID", report.RequestID)
			w.WriteHeader(http.StatusOK)
			w.Write(data)
		})
	}
}

// AdminFromClaims builds an AdminGate that checks for admin or super_admin
// roles in the JWT claims or core.AuthClaims stored on context.
func AdminFromClaims() AdminGate {
	return func(r *http.Request) bool {
		// Try core.AuthClaims first (external API key path).
		if ac := core.GetClaims(r.Context()); ac != nil {
			for _, role := range ac.Roles {
				if role == "admin" || role == "super_admin" {
					return true
				}
			}
		}
		return false
	}
}

// captureWriter captures the response body, status code, and headers without
// forwarding them to the real writer (debug report replaces them).
type captureWriter struct {
	http.ResponseWriter
	buf          *bytes.Buffer
	t            *Tracer
	hdr          http.Header
	status       int
	bytesWritten int64
}

func (c *captureWriter) Header() http.Header { return c.hdr }

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.bytesWritten += int64(len(b))
	return c.buf.Write(b)
}

func (c *captureWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// HookBus debug wrapper

// HookBus wraps a core.HookBus to record per-Publish debug segments.
// When a debug tracer is present in the context, each Publish call is timed
// and the handler execution duration is recorded.
type HookBus struct {
	inner core.HookBus
}

// NewHookBus returns a debug-recording HookBus wrapper.
func NewHookBus(inner core.HookBus) *HookBus {
	return &HookBus{inner: inner}
}

// Subscribe delegates directly.
func (h *HookBus) Subscribe(schema string, event core.EventType, handler core.EventHandler) core.Subscription {
	return h.inner.Subscribe(schema, event, handler)
}

// On delegates directly to the inner HookBus.
func (h *HookBus) On(event string, handler core.SystemEventHandler) core.Subscription {
	return h.inner.On(event, handler)
}

// HookPublisher debug wrapper

// HookPublisher wraps a core.HookPublisher to record per-Publish debug segments.
type HookPublisher struct {
	inner core.HookPublisher
}

// NewHookPublisher returns a debug-recording HookPublisher wrapper.
func NewHookPublisher(inner core.HookPublisher) *HookPublisher {
	return &HookPublisher{inner: inner}
}

// Publish records timing when a debug tracer is present, measuring the
// full dispatch duration including all handler execution time.
func (h *HookPublisher) Publish(ctx context.Context, event core.Event) error {
	t := TracerFromCtx(ctx)
	if t != nil {
		start := time.Now()
		err := h.inner.Publish(ctx, event)
		dur := time.Since(start)
		name := string(event.Type) + "/" + event.Schema
		t.Record(name, KindHookEvent, dur, HookEventDetail{
			EventType: string(event.Type),
			Schema:    event.Schema,
			Duration:  float64(dur.Microseconds()) / 1000.0,
		})
		return err
	}
	return h.inner.Publish(ctx, event)
}

// Unwrap returns the inner HookPublisher.
func (h *HookPublisher) Unwrap() core.HookPublisher { return h.inner }

// Hijack passes a connection upgrade through to the writer underneath.
// Without it a WebSocket or SSE handler mounted below this middleware cannot
// take the socket, and the handshake fails with the response already committed.
func (c *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Flush lets a streaming handler below this middleware push bytes out
// rather than sit in the buffer until the request ends.
func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
