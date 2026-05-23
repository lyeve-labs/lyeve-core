package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// TestEndToEnd_CorrelationIDTraceability verifies that a single request ID
// flows through the entire pipeline: HTTP -> ctx -> logs -> spans -> response.
func TestEndToEnd_CorrelationIDTraceability(t *testing.T) {
	// Set up a trace exporter to capture spans.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Set up a log buffer to capture structured logs.
	var logBuf bytes.Buffer
	logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	enrichHandler := logging.NewEnrichHandler(logHandler, nil)
	logging.SetGlobalEnrichHandler(enrichHandler)
	defer logging.SetGlobalEnrichHandler(nil)
	slog.SetDefault(enrichHandler.Logger())

	// Build a realistic middleware stack.
	r := chi.NewRouter()
	r.Use(chimw.RealIP)
	r.Use(apimw.CorrelationID()) // the correlation middleware
	// Note: tracing.HTTPMiddleware would normally go here, but we're testing
	// correlation ID independently. In production, both are present.

	// Capture what the handler sees.
	var handlerSawID string
	var handlerSawLogID string

	r.Get("/api/test", func(w http.ResponseWriter, r *http.Request) {
		handlerSawID = apimw.CorrelationIDFromCtx(r.Context())
		handlerSawLogID = logging.RequestIDFromCtx(r.Context())

		// Log something: the EnrichHandler should inject the request ID.
		slog.InfoContext(r.Context(), "handler executing",
			"action", "test",
		)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Test 1: Inbound X-Request-ID
	t.Run("inbound X-Request-ID flows through", func(t *testing.T) {
		logBuf.Reset()
		exporter.Reset()

		const inboundID = "test-correlation-id-abc123"

		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req.Header.Set("X-Request-ID", inboundID)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		// Response header.
		assert.Equal(t, inboundID, rec.Header().Get("X-Request-ID"),
			"response should echo X-Request-ID")

		// Handler saw the ID on context.
		assert.Equal(t, inboundID, handlerSawID,
			"handler should see correlation ID from context")
		assert.Equal(t, inboundID, handlerSawLogID,
			"handler should see correlation ID from logging context")

		// Log contains the request ID.
		logOutput := logBuf.String()
		assert.Contains(t, logOutput, inboundID,
			"structured log should contain the correlation ID")
		assert.Contains(t, logOutput, "request_id",
			"log should have request_id field")
	})

	// Test 2: W3C traceparent -> trace-id as correlation ID
	t.Run("traceparent trace-id becomes correlation ID", func(t *testing.T) {
		logBuf.Reset()
		exporter.Reset()

		const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
		const expectedTraceID = "0af7651916cd43dd8448eb211c80319c"

		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req.Header.Set("traceparent", traceparent)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, expectedTraceID, rec.Header().Get("X-Request-ID"),
			"response should echo trace-id as X-Request-ID")
		assert.Equal(t, expectedTraceID, handlerSawID,
			"handler should see trace-id as correlation ID")
	})

	// Test 3: No inbound headers -> generated ID
	t.Run("generated ID when no headers", func(t *testing.T) {
		logBuf.Reset()
		exporter.Reset()

		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		respID := rec.Header().Get("X-Request-ID")
		assert.NotEmpty(t, respID, "should generate an ID")
		assert.Len(t, respID, 32, "should be 128-bit hex")
		assert.Equal(t, respID, handlerSawID,
			"handler should see the generated ID")
	})

	// Test 4: X-Request-ID takes priority over traceparent
	t.Run("X-Request-ID priority over traceparent", func(t *testing.T) {
		logBuf.Reset()
		exporter.Reset()

		const inboundID = "explicit-id-wins"
		const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req.Header.Set("X-Request-ID", inboundID)
		req.Header.Set("traceparent", traceparent)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, inboundID, rec.Header().Get("X-Request-ID"),
			"X-Request-ID should take priority")
		assert.Equal(t, inboundID, handlerSawID)
	})
}

// TestEndToEnd_LogEnrichment verifies that the EnrichHandler injects the
// correlation ID into every log line when the correlation middleware is active.
func TestEndToEnd_LogEnrichment(t *testing.T) {
	var logBuf bytes.Buffer
	logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	enrichHandler := logging.NewEnrichHandler(logHandler, nil)
	logging.SetGlobalEnrichHandler(enrichHandler)
	defer logging.SetGlobalEnrichHandler(nil)
	slog.SetDefault(enrichHandler.Logger())

	r := chi.NewRouter()
	r.Use(apimw.CorrelationID())

	r.Get("/log-test", func(w http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "test log message")
		w.WriteHeader(http.StatusOK)
	})

	const requestID = "log-enrichment-test-id"

	req := httptest.NewRequest(http.MethodGet, "/log-test", nil)
	req.Header.Set("X-Request-ID", requestID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Parse the log line.
	logOutput := logBuf.String()
	require.NotEmpty(t, logOutput, "should have log output")

	var logEntry map[string]any
	require.NoError(t, json.Unmarshal([]byte(logOutput), &logEntry))

	assert.Equal(t, requestID, logEntry["request_id"],
		"log entry should contain the correlation ID")
	assert.Equal(t, "test log message", logEntry["msg"],
		"log entry should contain the message")
}

// TestEndToEnd_CorrelationIDInResponseHeaders verifies the correlation ID
// appears in response headers for all response types (success, error, etc.).
func TestEndToEnd_CorrelationIDInResponseHeaders(t *testing.T) {
	r := chi.NewRouter()
	r.Use(apimw.CorrelationID())

	r.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"200 OK", "/ok", http.StatusOK},
		{"500 Error", "/error", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := "header-test-" + tt.name

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("X-Request-ID", requestID)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, requestID, rec.Header().Get("X-Request-ID"),
				"X-Request-ID should be in response headers for %d", tt.status)
			assert.Equal(t, tt.status, rec.Code)
		})
	}
}
