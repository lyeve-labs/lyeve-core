package middleware

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

const (
	// captureMaxBody is the maximum bytes of request or response body that
	// will be captured into memory.  Bodies exceeding this limit have their
	// captured copy truncated to captureMaxBody bytes and the CaptureEntry
	// is stamped with Truncated=true.
	//
	// This is defense-in-depth: the outer MaxBodySize middleware at the
	// router layer caps the total request body at MaxBodyBytes (default
	// 10 MiB), and the response writer is otherwise unbounded.  This
	// ceiling ensures capture itself never balloons memory regardless of
	// middleware ordering or configuration.
	captureMaxBody = 1 << 20 // 1 MiB
)

// RequestCapture wraps the handler to capture full request and response
// data, then passes the entry to the provided CaptureSink. A nil sink
// makes the middleware a no-op.
//
// Both request and response bodies are capped at captureMaxBody (1 MiB).
// Bodies larger than this are truncated in the captured entry and the
// Truncated flag is set so downstream consumers know they have a prefix,
// not the full body.
//
// The body is read through an io.LimitReader so the middleware never
// balloons memory even when the upstream MaxBodySize middleware is
// disabled: a defense-in-depth guard.
//
// The sink's Capture method is called on the goroutine that services the
// request: it should be fast (insert a row, append to a buffer, etc.).
// Long-running work should be dispatched to a background goroutine by
// the sink implementation.
func RequestCapture(sink observability.CaptureSink) func(http.Handler) http.Handler {
	return RequestCaptureWithholding(sink, nil)
}

// RequestCaptureWithholding is RequestCapture with one more reason to keep a
// request's bodies out of the capture: withholds reports whether the route
// the request is addressed to declared them sensitive. The engine's own
// credential roots are withheld whatever it answers, and a nil withholds
// leaves them as the only test.
func RequestCaptureWithholding(sink observability.CaptureSink, withholds func(*http.Request) bool) func(http.Handler) http.Handler {
	return RequestCaptureWithPolicy(sink, withholds, nil)
}

// RequestCaptureWithPolicy is RequestCaptureWithholding with a policy that
// decides, once the handler has run and the tenant is known, whether the
// request is stored and for how long. A nil policy stores every request for
// core.DefaultCaptureTTL, which is what an install without one gets.
//
// The bodies are still read before the handler runs, because the policy
// cannot be asked until the tenant is resolved below this middleware. A
// request the policy declines is never cloned or handed to the sink.
func RequestCaptureWithPolicy(sink observability.CaptureSink, withholds func(*http.Request) bool, policy core.CapturePolicy) func(http.Handler) http.Handler {
	if sink == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read the request body for capture, then replace r.Body with a
			// fresh reader so the handler sees the original. Defense-in-depth
			// LimitReader bounds memory even if MaxBodySize is disabled.
			var reqBody []byte
			var reqTruncated bool
			if r.Body != nil && r.Body != http.NoBody {
				// Read captureMaxBody+1 bytes: the extra byte detects
				// truncation without a second syscall.
				limited := io.LimitReader(r.Body, captureMaxBody+1)
				fullBody, readErr := io.ReadAll(limited)
				if readErr != nil {
					slog.Debug("request capture: body read failed", "err", readErr)
				}
				if len(fullBody) > captureMaxBody {
					reqBody = fullBody[:captureMaxBody]
					reqTruncated = true
					// Stitch a reader over what was already read plus the
					// unconsumed remainder so the handler sees the full payload.
					r.Body = io.NopCloser(io.MultiReader(
						bytes.NewReader(fullBody),
						r.Body,
					))
				} else {
					// Fully consumed: replace with a reader over the captured bytes.
					reqBody = fullBody
					r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(fullBody))
				}
			}

			start := time.Now()
			crw := &captureResponseWriter{
				ResponseWriter: w,
				body:           &captureBuffer{},
			}
			next.ServeHTTP(crw, r)
			dur := time.Since(start)

			tenantID := captureTenantID(r)
			ttl := core.DefaultCaptureTTL
			if policy != nil {
				keep, capture := policy.CaptureFor(tenantID, r.Method, r.URL.Path)
				if !capture {
					return
				}
				if keep > 0 {
					ttl = keep
				}
			}

			respBody := crw.body.Bytes()
			respTruncated := crw.body.Truncated()
			if captureWithholdsBodies(r.URL.Path) || (withholds != nil && withholds(r)) {
				reqBody, respBody = nil, nil
			}

			_ = sink.Capture(observability.CaptureEntry{ // err suppressed: capture sink is best-effort
				Timestamp:       start,
				Method:          r.Method,
				URL:             r.URL.String(),
				RequestHeaders:  redactHeaders(cloneHeaders(r.Header)),
				RequestBody:     reqBody,
				StatusCode:      crw.statusCode,
				ResponseHeaders: redactHeaders(cloneHeaders(crw.Header())),
				ResponseBody:    respBody,
				Duration:        dur,
				TTL:             ttl,
				TenantID:        tenantID,
				Truncated:       reqTruncated || respTruncated,
			})
		})
	}
}

// captureResponseWriter records status and body for capture while passing
// writes through. Implements http.Flusher and http.Pusher for handler
// compatibility. Body buffer capped at captureMaxBody. Excess is discarded
// from the captured copy but still forwarded.
type captureResponseWriter struct {
	http.ResponseWriter
	statusCode int
	body       *captureBuffer
	wroteHdr   bool
}

// WriteHeader records the status code then delegates to the real writer.
func (c *captureResponseWriter) WriteHeader(code int) {
	if !c.wroteHdr {
		c.statusCode = code
		c.wroteHdr = true
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write records the bytes in the capture body buffer and passes them through.
func (c *captureResponseWriter) Write(b []byte) (int, error) {
	if !c.wroteHdr {
		c.WriteHeader(http.StatusOK)
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer for http.ResponseController.
func (c *captureResponseWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// Flush delegates to the underlying Flusher if supported.
func (c *captureResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// captureBuffer is a bounded byte buffer that silently drops writes past
// captureMaxBody and tracks whether truncation occurred.
type captureBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

// Write copies up to captureMaxBody bytes into the buffer, silently
// dropping excess.
func (b *captureBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		// Already at cap: drop writes silently (no error to the caller).
		return len(p), nil
	}
	avail := captureMaxBody - b.buf.Len()
	if len(p) > avail {
		b.buf.Write(p[:avail])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Bytes returns the captured body bytes.
func (b *captureBuffer) Bytes() []byte { return b.buf.Bytes() }

// Truncated reports whether the captured body exceeds captureMaxBody.
func (b *captureBuffer) Truncated() bool { return b.truncated }

// captureTenantID resolves the tenant a request was scoped to for the stored
// capture. Tenancy is resolved partway down the chain (TenantHeader puts the
// tenant on a context only the handlers below it see), so this middleware,
// which wraps the whole chain, cannot read TenantIDFromCtx from the request it
// was handed. The outer structured logger installs a TenantSlot before the
// chain runs and TenantHeader fills it. Read that back so captures are stored
// with the tenant the request actually ran as rather than an empty one.
func captureTenantID(r *http.Request) string {
	if slot := core.TenantSlotFrom(r.Context()); slot != nil {
		return slot.Get()
	}
	return ""
}

// redactHeaders returns a copy with sensitive headers replaced by "[REDACTED]".
// Uses observability.SensitiveHeaders so middleware and plugins stay in sync.
func redactHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := cloneHeaders(h)
	for _, name := range observability.SensitiveHeaders {
		if _, ok := out[name]; ok {
			out[name] = []string{"[REDACTED]"}
		}
	}
	return out
}

// cloneHeaders returns a shallow copy of h.
func cloneHeaders(h http.Header) http.Header {
	c := make(http.Header, len(h))
	for k, v := range h {
		c[k] = v
	}
	return c
}

// Hijack passes a connection upgrade through to the writer underneath.
// Without it a WebSocket or SSE handler mounted below this middleware cannot
// take the socket, and the handshake fails with the response already committed.
func (c *captureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// credentialRoots are the first path segments under /api/admin and /api/v1
// whose bodies carry a credential on the routes the engine serves: a
// password, a setup token, an admin token shown once and the password or MFA
// code that confirms issuing it, and the engine configuration with its
// secrets. A sink redacts by field name, and a name it does not know is
// stored in the clear for any tenant admin to read, so these bodies never
// reach it. A route the engine does not serve says so in its own
// declaration, which RequestCaptureWithholding reads.
var credentialRoots = map[string]bool{
	"auth":         true,
	"setup":        true,
	"config":       true,
	"admin-tokens": true,
}

// captureWithholdsBodies reports whether a request's bodies are kept out of
// the capture. The method, URL, headers and status are still recorded.
func captureWithholdsBodies(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/admin/")
	if !ok {
		rest, ok = strings.CutPrefix(path, "/api/v1/")
	}
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(rest, "/")
	if credentialRoots[first] {
		return true
	}
	rest = strings.TrimSuffix(rest, "/")
	switch first {
	case "users":
		return strings.HasSuffix(rest, "/password")
	case "plugins":
		// A plugin's configuration holds its own secrets: an SMTP password,
		// a provider key.
		return strings.HasSuffix(rest, "/config") || strings.Contains(rest, "/config/")
	}
	return false
}
