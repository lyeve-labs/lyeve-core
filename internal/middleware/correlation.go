package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
)

type correlationKey string

const (
	ctxKeyCorrelationID correlationKey = "correlation_id"
	ctxKeyTraceparent   correlationKey = "traceparent"
)

// CorrelationIDFromCtx returns the correlation ID from ctx, or "" if not set or ctx is nil.
func CorrelationIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyCorrelationID).(string); ok {
		return v
	}
	return ""
}

// TraceparentFromCtx returns the raw W3C traceparent from ctx, or "" if not set or ctx is nil.
func traceparentFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyTraceparent).(string); ok {
		return v
	}
	return ""
}

// CorrelationID returns middleware that ensures every request carries a
// correlation ID through the pipeline. It accepts an inbound X-Request-ID
// or extracts a W3C traceparent trace-id, generating a 128-bit hex ID
// when neither is present. The ID is stored on context (CorrelationIDFromCtx),
// echoed as X-Request-ID in the response, set on chi's request-ID context,
// and linked to the current OTel span. Place early in the middleware chain,
// before logging and tracing.
func CorrelationID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := resolveCorrelationID(r)

			ctx := context.WithValue(r.Context(), ctxKeyCorrelationID, id)
			ctx = context.WithValue(ctx, middleware.RequestIDKey, id)

			if tp := r.Header.Get("traceparent"); tp != "" {
				ctx = context.WithValue(ctx, ctxKeyTraceparent, tp)
			}

			ctx = logging.WithRequestID(ctx, id)

			if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
				span.SetAttributes(attribute.String("lyeve.request_id", id))
			}

			w.Header().Set("X-Request-ID", id)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// maxCorrelationIDLen bounds an inbound X-Request-ID.
//
// The value is echoed back as a response header and written to every log line
// the request produces, so an unbounded one is amplified rather than merely
// stored: a 16 KB request header returns as a 16 KB response header, which
// overruns the header budget most clients allocate and makes the whole response
// unparseable. The caller then cannot read even the status line.
//
// 128 clears every format in the wild by a wide margin. A UUID is 36
// characters, a W3C trace-id 32, and the generated ID below is 32.
const maxCorrelationIDLen = 128

// acceptableCorrelationID reports whether an inbound ID is safe to adopt: bounded,
// and printable ASCII throughout. A control character in a value destined for a
// response header is a response-splitting attempt, and nothing outside this set
// belongs in a correlation ID. A value that fails here is not truncated into
// shape, because a silently shortened ID correlates the request to nothing and
// still reflects whatever the caller chose. It is dropped, and the caller gets a
// generated ID instead.
func acceptableCorrelationID(s string) bool {
	if s == "" || len(s) > maxCorrelationIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// resolveCorrelationID picks the best correlation ID: X-Request-ID, then
// W3C traceparent trace-id, then a generated 128-bit hex ID.
func resolveCorrelationID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); acceptableCorrelationID(id) {
		return id
	}

	// W3C traceparent: version-traceid-parentid-traceflags.
	if tp := r.Header.Get("traceparent"); tp != "" {
		if traceID := extractTraceID(tp); traceID != "" {
			return traceID
		}
	}

	return generateRequestID()
}

// extractTraceID returns the 32-hex-char trace-id from a W3C traceparent
// header, or "" if the format is invalid or the trace-id is all zeros.
func extractTraceID(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		return ""
	}
	if len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	if parts[1] == "00000000000000000000000000000000" {
		return ""
	}
	return parts[1]
}

// generateRequestID creates a new 128-bit random hex ID.
func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
