package tracing

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	apitrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// test helpers

func testTraceProvider() (*tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return exp, tp
}

func resetTracing() {
	otel.SetTracerProvider(noop.NewTracerProvider())
}

func spanAttrs(s tracetest.SpanStub) map[string]string {
	m := make(map[string]string)
	for _, attr := range s.Attributes {
		m[string(attr.Key)] = attr.Value.String()
	}
	return m
}

// HTTPMiddleware

func TestHTTPMiddleware_SpanCreated(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-service")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req.Header.Set("User-Agent", "TestAgent/1.0")
	req.Host = "api.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]

	assert.Equal(t, "GET /api/v1/content/posts", span.Name)
	assert.Equal(t, apitrace.SpanKindServer, span.SpanKind)

	attrs := spanAttrs(span)
	assert.Equal(t, "GET", attrs["http.request.method"])
	assert.Equal(t, "/api/v1/content/posts", attrs["url.path"])
	assert.Equal(t, "/api/v1/content/posts", attrs["url.full"])
	assert.Equal(t, "http", attrs["url.scheme"])
	assert.Equal(t, "api.example.com", attrs["server.address"])
	assert.Equal(t, "TestAgent/1.0", attrs["user_agent.original"])
	assert.Equal(t, "200", attrs["http.response.status_code"])
}

func TestHTTPMiddleware_ErrorStatus(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-service")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]

	assert.Equal(t, "GET /api/v1/missing", span.Name)
	attrs := spanAttrs(span)
	assert.Equal(t, "404", attrs["http.response.status_code"])
	assert.Equal(t, "Error", span.Status.Code.String())
	assert.Equal(t, "Not Found", span.Status.Description)
}

func TestHTTPMiddleware_5xxError(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-service")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/data", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]

	assert.Equal(t, "POST /api/v1/data", span.Name)
	assert.Equal(t, "Internal Server Error", span.Status.Description)
}

func TestHTTPMiddleware_NoopWhenNoProvider(t *testing.T) {
	resetTracing()

	handler := HTTPMiddleware("test-service")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// schemeFromReq

func TestSchemeFromReq(t *testing.T) {
	tests := []struct {
		name     string
		tls      bool
		header   string
		expected string
	}{
		{"bare http", false, "", "http"},
		{"https via TLS", true, "", "https"},
		{"https via X-Forwarded-Proto", false, "https", "https"},
		{"http via X-Forwarded-Proto", false, "http", "http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tt.header != "" {
				req.Header.Set("X-Forwarded-Proto", tt.header)
			}
			assert.Equal(t, tt.expected, schemeFromReq(req))
		})
	}
}

// DBTracer

func TestDBTracer_Span(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test-service")

	_, span := tr.Span(context.Background(), "query", "SELECT id, name FROM users WHERE id = $1")
	span.End() // call directly: GetSpans checks the span list after this

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	s := spans[0]

	assert.Equal(t, "db.query", s.Name)
	assert.Equal(t, apitrace.SpanKindClient, s.SpanKind)

	attrs := spanAttrs(s)
	assert.Equal(t, "query", attrs["db.operation"])
	assert.Contains(t, attrs["db.statement"], "SELECT id, name")
	assert.Equal(t, "sql", attrs["db.system"])
}

func TestDBTracer_SpanExec(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test-service")

	_, span := tr.Span(context.Background(), "exec", "INSERT INTO users (name) VALUES ($1)")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "db.exec", spans[0].Name)
}

func TestDBTracer_SpanBegin(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test-service")

	_, span := tr.Span(context.Background(), "begin", "BEGIN")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "db.begin", spans[0].Name)
}

// RecordError

func TestRecordError(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test")
	_, span := tr.Span(context.Background(), "query", "SELECT 1")

	ok := RecordError(span, fmt.Errorf("simulated error"))
	assert.True(t, ok)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "Error", spans[0].Status.Code.String())
	assert.Equal(t, "simulated error", spans[0].Status.Description)
}

func TestRecordError_Nil(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test")
	_, span := tr.Span(context.Background(), "query", "SELECT 1")

	ok := RecordError(span, nil)
	assert.False(t, ok)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "Unset", spans[0].Status.Code.String())
}

// Helper functions

func TestTruncateSQL(t *testing.T) {
	tests := []struct {
		sql    string
		maxLen int
		expect string
	}{
		{"SELECT 1", 80, "SELECT 1"},
		{"SELECT id, name FROM users", 10, "SELECT id,..."},
		{"SHORT", 80, "SHORT"},
		{"", 80, ""},
	}
	for _, tt := range tests {
		name := tt.sql
		if len(name) > 8 {
			name = name[:8]
		}
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.expect, TruncateSQL(tt.sql, tt.maxLen))
		})
	}
}

func TestSQLSummary(t *testing.T) {
	sql := "SELECT id, name, email, created_at, updated_at FROM users WHERE status = 'active' ORDER BY created_at DESC"
	summary := SQLSummary(sql)
	assert.LessOrEqual(t, len(summary), 83)
	assert.Contains(t, summary, "SELECT")
}

// DBQuerySpan

func TestDBQuerySpan_NilTracer(t *testing.T) {
	ctx := context.Background()
	ctx2, span := DBQuerySpan(ctx, nil, "query", "SELECT 1")
	assert.Equal(t, ctx, ctx2)
	assert.False(t, span.SpanContext().IsValid())
}

func TestDBQuerySpan_WithTracer(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	tr := NewDBTracer("test")
	_, span := DBQuerySpan(context.Background(), tr, "ListPosts", "SELECT id, title FROM posts ORDER BY created_at DESC")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "db.ListPosts", spans[0].Name)
}

// PluginSpan

func TestPluginSpan(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	_, span := PluginSpan(context.Background(), "content", "Create", "POST /api/v1/content/posts")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	s := spans[0]

	assert.Equal(t, "content.Create", s.Name)
	assert.Equal(t, apitrace.SpanKindInternal, s.SpanKind)

	attrs := spanAttrs(s)
	assert.Equal(t, "content", attrs["plugin.name"])
	assert.Equal(t, "Create", attrs["plugin.operation"])
	assert.Equal(t, "POST /api/v1/content/posts", attrs["http.route"])
}

func TestPluginSpan_NoopWhenDisabled(t *testing.T) {
	resetTracing()

	_, span := PluginSpan(context.Background(), "my-plugin", "List", "GET /api/v1/items")
	span.End()

	assert.False(t, span.SpanContext().IsValid())
}

// Integration: full trace context propagation through middleware

func TestTraceContextPropagation(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	var capturedTraceID string
	var capturedSpanID string

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := apitrace.SpanFromContext(r.Context())
		if span.SpanContext().IsValid() {
			capturedTraceID = span.SpanContext().TraceID().String()
			capturedSpanID = span.SpanContext().SpanID().String()
		}
		w.WriteHeader(http.StatusOK)
	})

	wrapped := HTTPMiddleware("integration-test")(inner)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	assert.NotEmpty(t, capturedTraceID)
	assert.NotEmpty(t, capturedSpanID)
	assert.Equal(t, spans[0].SpanContext.TraceID().String(), capturedTraceID)
	assert.Equal(t, spans[0].SpanContext.SpanID().String(), capturedSpanID)
}

func TestResponseWriterUnwrap(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &tracingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	unwrapped := rw.Unwrap()
	assert.Same(t, w, unwrapped)
}

func TestResponseWriterDefaultStatus(t *testing.T) {
	rw := &tracingResponseWriter{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
	_, _ = rw.Write([]byte("ok"))

	assert.Equal(t, http.StatusOK, rw.statusCode)
}
