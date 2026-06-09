package tracing

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-chi/chi/v5"
)

// HTTPMiddleware returns a chi-compatible middleware providing W3C trace
// context propagation and per-request tracing. On each request it:
//
//  1. Extracts incoming trace context (traceparent, tracestate) from request headers
//  2. Creates a SERVER span named "{METHOD} {path}"
//  3. Records standard HTTP semantic conventions + user agent on the span
//  4. Injects the span context into response headers for downstream propagation
//  5. Records the HTTP status code and sets span error status on 4xx/5xx
//  6. Enriches the span with tenant_id (from context) and http.route (from chi)
//
// When the global TracerProvider is the SDK noop default (tracing disabled),
// the middleware is a transparent pass-through with near-zero overhead.
func HTTPMiddleware(serviceName string) func(http.Handler) http.Handler {
	propagator := otel.GetTextMapPropagator()
	tracer := otel.Tracer(serviceName, trace.WithInstrumentationVersion("v1"))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			spanName := r.Method + " " + r.URL.Path
			ctx, span := tracer.Start(ctx, spanName,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("url.path", r.URL.Path),
					attribute.String("url.full", r.URL.String()),
					attribute.String("url.scheme", schemeFromReq(r)),
					attribute.String("server.address", r.Host),
					attribute.String("user_agent.original", r.UserAgent()),
				),
			)
			defer span.End()

			propagator.Inject(ctx, propagation.HeaderCarrier(w.Header()))

			ww := &tracingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
			r = r.WithContext(ctx)

			next.ServeHTTP(ww, r)

			// Enrich span with attributes available after handler chain runs.
			attrs := []attribute.KeyValue{
				attribute.Int("http.response.status_code", ww.statusCode),
			}
			// Tenant ID: use raw context key to avoid importing core
			// (which creates an import cycle through config -> db -> tracing).
			if tenantID := tenantIDFromCtx(r.Context()); tenantID != "" {
				attrs = append(attrs, attribute.String("tenant.id", tenantID))
			}
			// HTTP route pattern: set by chi after handler matching.
			if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
				attrs = append(attrs, attribute.String("http.route", rctx.RoutePattern()))
			}
			span.SetAttributes(attrs...)
			if ww.statusCode >= 400 {
				span.SetStatus(codes.Error, http.StatusText(ww.statusCode))
			}

			slog.Debug("http span",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.statusCode,
				"trace_id", span.SpanContext().TraceID().String(),
				"span_id", span.SpanContext().SpanID().String(),
			)
		})
	}
}

// tracingResponseWriter captures the HTTP status code for span recording.
type tracingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *tracingResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController access.
func (rw *tracingResponseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush propagates to the underlying writer. This middleware wraps every request
// on both routers, so without a Flush the flush chain dies here and every
// streaming response (SSE) buffers until the connection closes.
func (rw *tracingResponseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// schemeFromReq returns "https" when the request uses TLS or has an
// X-Forwarded-Proto: https header. Otherwise "http".
func schemeFromReq(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

// Hijack passes a connection upgrade through to the writer underneath.
// Without it a WebSocket or SSE handler mounted below this middleware cannot
// take the socket, and the handshake fails with the response already committed.
func (rw *tracingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
