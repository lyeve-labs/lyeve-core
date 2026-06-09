package tracing

//lint:file-ignore SA1029 Using string context keys that must match core.TenantContextKey.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	apitrace "go.opentelemetry.io/otel/trace"
)

// TenantSampler

func TestTenantSampler_AlwaysSample(t *testing.T) {
	ts := NewTenantSampler(nil, 1.0)
	p := sdktrace.SamplingParameters{ParentContext: context.Background()}
	result := ts.ShouldSample(p)
	assert.Equal(t, sdktrace.RecordAndSample, result.Decision)
}

func TestTenantSampler_AlwaysDrop(t *testing.T) {
	ts := NewTenantSampler(nil, 0.0)
	p := sdktrace.SamplingParameters{ParentContext: context.Background()}
	result := ts.ShouldSample(p)
	assert.Equal(t, sdktrace.Drop, result.Decision)
}

func TestTenantSampler_PerTenantRate(t *testing.T) {
	rates := map[string]float64{"acme": 1.0, "bronze": 0.0}
	ts := NewTenantSampler(rates, 0.5)

	ctx := context.WithValue(context.Background(), tenantContextKey, "acme")
	result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
	assert.Equal(t, sdktrace.RecordAndSample, result.Decision)

	ctx = context.WithValue(context.Background(), tenantContextKey, "bronze")
	result = ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
	assert.Equal(t, sdktrace.Drop, result.Decision)
}

func TestTenantSampler_FallbackToGlobal(t *testing.T) {
	rates := map[string]float64{"acme": 0.1}
	ts := NewTenantSampler(rates, 1.0)

	ctx := context.WithValue(context.Background(), tenantContextKey, "unknown")
	result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
	assert.Equal(t, sdktrace.RecordAndSample, result.Decision)
}

func TestTenantSampler_NoTenant(t *testing.T) {
	ts := NewTenantSampler(nil, 0.5)
	p := sdktrace.SamplingParameters{ParentContext: context.Background()}
	assert.NotPanics(t, func() { ts.ShouldSample(p) })
}

func TestTenantSampler_UpdateRates(t *testing.T) {
	ts := NewTenantSampler(map[string]float64{"old": 0.1}, 1.0)

	ctx := context.WithValue(context.Background(), tenantContextKey, "new")
	result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
	assert.Equal(t, sdktrace.RecordAndSample, result.Decision)

	ts.UpdateRates(map[string]float64{"new": 0.0}, 0.0)
	result = ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
	assert.Equal(t, sdktrace.Drop, result.Decision)
}

func TestTenantSampler_Concurrent(t *testing.T) {
	ts := NewTenantSampler(nil, 1.0)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), tenantContextKey, fmt.Sprintf("t-%d", i))
			ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
			ts.UpdateRates(map[string]float64{}, 0.5)
		}()
	}
	wg.Wait()
}

// HTTPMiddleware tenant_id + http.route enrichment

func TestHTTPMiddleware_TenantAttribute(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-tenant")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantContextKey, "acme"))
	req.Host = "api.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := spanAttrs(spans[0])
	assert.Equal(t, "acme", attrs["tenant.id"])
}

func TestHTTPMiddleware_NoTenantWhenMissing(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-tenant")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := spanAttrs(spans[0])
	_, hasTenant := attrs["tenant.id"]
	assert.False(t, hasTenant, "tenant.id should not be set when no tenant in context")
}

func TestHTTPMiddleware_RouteAttribute(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	mw := HTTPMiddleware("test-route")
	r := chi.NewRouter()
	r.Use(mw)
	r.Get("/api/v1/posts/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/posts/42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := spanAttrs(spans[0])
	assert.Equal(t, "/api/v1/posts/{id}", attrs["http.route"])
}

func TestHTTPMiddleware_W3CResponseHeaders(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	handler := HTTPMiddleware("test-w3c")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	traceParent := rec.Header().Get("traceparent")
	assert.NotEmpty(t, traceParent, "traceparent should be set in response headers")
	assert.Contains(t, traceParent, "00-", "traceparent must have version prefix")

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
}

// TracedTransport

func TestTracedTransport_InjectsTraceParent(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tpHeader := r.Header.Get("traceparent")
		w.Header().Set("X-Traceparent-Echo", tpHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewTracedTransport(http.DefaultTransport, "test-client")
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	echo := resp.Header.Get("X-Traceparent-Echo")
	assert.NotEmpty(t, echo, "server must receive traceparent header")
	assert.Contains(t, echo, "00-")

	spans := exp.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)
	found := false
	for _, s := range spans {
		if s.SpanKind == apitrace.SpanKindClient {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a CLIENT span")
}

func TestTracedTransport_PropagatesContext(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	ctx, serverSpan := otel.Tracer("test-parent").Start(context.Background(), "parent-span")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewTracedTransport(http.DefaultTransport, "test-client")
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// End parent so both spans are visible to the in-memory exporter.
	serverSpan.End()

	spans := exp.GetSpans()
	require.GreaterOrEqual(t, len(spans), 2)

	traceID := spans[0].SpanContext.TraceID()
	for _, s := range spans[1:] {
		assert.Equal(t, traceID, s.SpanContext.TraceID(), "all spans must share trace ID")
	}
}

func TestTracedTransport_ErrorSpan(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	transport := NewTracedTransport(http.DefaultTransport, "test-client")
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:65535/nope", nil)
	require.NoError(t, err)

	_, err = client.Do(req)
	require.Error(t, err) // expected: port not listening

	spans := exp.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)

	found := false
	for _, s := range spans {
		if s.SpanKind == apitrace.SpanKindClient {
			assert.Equal(t, "Error", s.Status.Code.String())
			found = true
			break
		}
	}
	assert.True(t, found, "expected a CLIENT span with error status")
}

// Integration: Propagate W3C traceparent through middleware -> handler

func TestFullTraceContextPropagation(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	var capturedTraceID, capturedSpanID string

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("test-integration"))
	r.Get("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		span := apitrace.SpanFromContext(r.Context())
		if span.SpanContext().IsValid() {
			capturedTraceID = span.SpanContext().TraceID().String()
			capturedSpanID = span.SpanContext().SpanID().String()
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	assert.NotEmpty(t, capturedTraceID)
	assert.NotEmpty(t, capturedSpanID)
	assert.Equal(t, spans[0].SpanContext.TraceID().String(), capturedTraceID)
	assert.Equal(t, spans[0].SpanContext.SpanID().String(), capturedSpanID)
}

func TestFullPropagation_WithTenant(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("test-tenant"))
	r.Get("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantContextKey, "acme"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := spanAttrs(spans[0])
	assert.Equal(t, "acme", attrs["tenant.id"])
	assert.Equal(t, "/api/data", attrs["http.route"])
	assert.Equal(t, "200", attrs["http.response.status_code"])
}

// Edge case: Host info extraction from traceparent

func TestTraceParentRoundtrip(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())

	var (
		incomingTraceID string
		outgoingTraceID string
	)

	ctx, upstreamSpan := otel.Tracer("upstream").Start(context.Background(), "upstream")
	upstreamTraceID := upstreamSpan.SpanContext().TraceID().String()
	headers := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(headers))
	upstreamSpan.End()

	incomingTraceID = upstreamTraceID

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("test-downstream"))
	r.Get("/api/downstream", func(w http.ResponseWriter, r *http.Request) {
		span := apitrace.SpanFromContext(r.Context())
		outgoingTraceID = span.SpanContext().TraceID().String()
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/downstream", nil)
	req.Header = headers
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.NotEmpty(t, incomingTraceID)
	assert.NotEmpty(t, outgoingTraceID)
	assert.Equal(t, incomingTraceID, outgoingTraceID, "trace ID must propagate across services")

	spans := exp.GetSpans()
	require.GreaterOrEqual(t, len(spans), 2)
	found := false
	for _, s := range spans {
		if s.SpanContext.TraceID().String() == outgoingTraceID && s.SpanKind == apitrace.SpanKindServer {
			assert.Equal(t, incomingTraceID, s.SpanContext.TraceID().String(), "downstream span must share trace ID")
			found = true
			break
		}
	}
	assert.True(t, found, "expected a downstream SERVER span with matching trace ID")
}

func TestConfigParsing_Smoke(t *testing.T) {
	t.Run("valid pairs", func(t *testing.T) {
		rates := map[string]float64{"acme": 1.0, "bronze": 0.1}
		ts := NewTenantSampler(rates, 1.0)

		ctx := context.WithValue(context.Background(), tenantContextKey, "acme")
		result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
		assert.Equal(t, sdktrace.RecordAndSample, result.Decision)

		ctx = context.WithValue(context.Background(), tenantContextKey, "bronze")
		assert.NotPanics(t, func() { ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx}) })
	})

	t.Run("empty rates", func(t *testing.T) {
		ts := NewTenantSampler(nil, 1.0)
		assert.NotNil(t, ts)
		assert.Equal(t, "TenantSampler", ts.Description())
	})

	t.Run("clamped rates", func(t *testing.T) {
		ts := NewTenantSampler(map[string]float64{"high": 2.5, "low": -0.5}, 1.0)

		ctx := context.WithValue(context.Background(), tenantContextKey, "high")
		result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
		assert.Equal(t, sdktrace.RecordAndSample, result.Decision)

		ctx = context.WithValue(context.Background(), tenantContextKey, "low")
		result = ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: ctx})
		assert.Equal(t, sdktrace.Drop, result.Decision)
	})

	t.Run("fallback clamped", func(t *testing.T) {
		ts := NewTenantSampler(nil, -1.0)
		result := ts.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background()})
		assert.Equal(t, sdktrace.RecordAndSample, result.Decision)
	})
}

// TestTenantContextKey matches core.TenantContextKey to catch drift.
func TestTenantContextKey(t *testing.T) {
	assert.Equal(t, "tenant_id", string(tenantContextKey))
}

// Trimmed prefixes for SQL/tracing output.
func TestSQLSummary_NotTooLong(t *testing.T) {
	longSQL := strings.Repeat("SELECT * FROM users WHERE id = 1; ", 10)
	summary := SQLSummary(longSQL)
	assert.LessOrEqual(t, len(summary), 83) // 80 + "..."
}
