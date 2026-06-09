package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// test doubles & helpers

// captureSink is an in-memory observability.LogSink that records every entry written.
type captureSink struct {
	mu      sync.Mutex
	name    string
	entries []observability.LogEntry
	closed  bool
}

func (s *captureSink) Write(e observability.LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
}

func (s *captureSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *captureSink) Name() string {
	if s.name != "" {
		return s.name
	}
	return "capture"
}

func (s *captureSink) snapshot() []observability.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observability.LogEntry(nil), s.entries...)
}

// validSpanCtx returns a context carrying a valid OpenTelemetry span context
// built from the given hex-encoded trace and span IDs.
func validSpanCtx(t *testing.T, traceHex, spanHex string) context.Context {
	t.Helper()
	tid, err := oteltrace.TraceIDFromHex(traceHex)
	require.NoError(t, err)
	sid, err := oteltrace.SpanIDFromHex(spanHex)
	require.NoError(t, err)
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: oteltrace.FlagsSampled,
	})
	return oteltrace.ContextWithSpanContext(context.Background(), sc)
}

// attrMap flattens []slog.Attr into a key->string map for easy assertions.
func attrMap(attrs []slog.Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	return m
}

// context.go: TraceID / SpanID / EnrichAttrs

func TestTraceID_FromSpan(t *testing.T) {
	ctx := validSpanCtx(t, "0102030405060708090a0b0c0d0e0f10", "0102030405060708")
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", logging.TraceID(ctx))
}

func TestTraceID_FallbackToRequestID(t *testing.T) {
	// No span -> falls back to the request ID.
	ctx := logging.WithRequestID(context.Background(), "req-xyz")
	assert.Equal(t, "req-xyz", logging.TraceID(ctx))
}

func TestTraceID_Empty(t *testing.T) {
	assert.Equal(t, "", logging.TraceID(context.Background()))
}

func TestSpanID_FromSpan(t *testing.T) {
	ctx := validSpanCtx(t, "0102030405060708090a0b0c0d0e0f10", "0102030405060708")
	assert.Equal(t, "0102030405060708", logging.SpanID(ctx))
}

func TestSpanID_Empty(t *testing.T) {
	assert.Equal(t, "", logging.SpanID(context.Background()))
}

func TestEnrichAttrs_AllSources(t *testing.T) {
	ctx := validSpanCtx(t, "0102030405060708090a0b0c0d0e0f10", "0102030405060708")
	ctx = context.WithValue(ctx, middleware.RequestIDKey, "req-42")
	ctx = core.WithTenantID(ctx, "acme")
	ctx = logging.WithPlugin(ctx, "widgets")

	m := attrMap(logging.EnrichAttrs(ctx))
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", m["trace_id"])
	assert.Equal(t, "0102030405060708", m["span_id"])
	assert.Equal(t, "req-42", m["request_id"])
	assert.Equal(t, "acme", m["tenant_id"])
	assert.Equal(t, "widgets", m["plugin"])
}

// handler.go: Middleware

func TestMiddleware_InjectsWorkingLogger(t *testing.T) {
	var buf bytes.Buffer
	h := logging.NewEnrichHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}), nil)

	var gotReqID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logging.Logger(r.Context()).Info("from-handler")
		gotReqID = logging.RequestIDFromCtx(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	logging.Middleware(h)(next).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	// The logger the middleware injected is backed by our handler -> the record
	// lands in buf. Proves WithLogger ran and produced a working logger.
	assert.Contains(t, buf.String(), "from-handler")
	// No chi RequestID middleware in front -> no request ID propagated.
	assert.Equal(t, "", gotReqID)
}

func TestMiddleware_PropagatesRequestID(t *testing.T) {
	h := logging.NewDefaultHandler()

	var gotReqID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqID = logging.RequestIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// chi's RequestID middleware sets a request ID that our middleware must
	// lift onto the logging context.
	chain := middleware.RequestID(logging.Middleware(h)(next))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/y", nil)
	chain.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, gotReqID, "request ID should be propagated onto the logging context")
}

// handler.go: Handle sink + tailer branches

func TestEnrichHandler_Handle_WritesToSink(t *testing.T) {
	var buf bytes.Buffer
	h := logging.NewEnrichHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}), nil)
	sink := &captureSink{}
	h.SetSink(sink)

	ctx := logging.WithPlugin(context.Background(), "widgets")
	rec := slog.NewRecord(time.Now(), slog.LevelWarn, "sink-msg", 0)
	rec.AddAttrs(slog.String("custom", "v1"))
	require.NoError(t, h.Handle(ctx, rec))

	entries := sink.snapshot()
	require.Len(t, entries, 1)
	got := entries[0]
	assert.Equal(t, "WARN", got.Level)
	assert.Equal(t, "sink-msg", got.Message)
	assert.Equal(t, "widgets", got.Attrs["plugin"])
	assert.NotEmpty(t, got.Timestamp)

	// Record-level attrs go to the inner handler (not the sink LogEntry).
	assert.Contains(t, buf.String(), "sink-msg")
	assert.Contains(t, buf.String(), "custom")
}

func TestEnrichHandler_Handle_BroadcastsToTailer(t *testing.T) {
	tailer := observability.NewLogTailer()
	_, ch := tailer.Subscribe(observability.TailFilter{})
	observability.SetGlobalTailer(tailer)
	t.Cleanup(func() { observability.SetGlobalTailer(nil) })

	var buf bytes.Buffer
	h := logging.NewEnrichHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}), nil)
	rec := slog.NewRecord(time.Now(), slog.LevelError, "tail-me", 0)
	require.NoError(t, h.Handle(context.Background(), rec))

	select {
	case e := <-ch:
		assert.Equal(t, "tail-me", e.Message)
		assert.Equal(t, "ERROR", e.Level)
	case <-time.After(2 * time.Second):
		t.Fatal("tailer did not receive the broadcast entry")
	}
}

// init.go: core wiring closures

func TestInitWiring_SetGlobalLogLeveler(t *testing.T) {
	t.Cleanup(func() { logging.SetGlobalEnrichHandler(nil) })

	logging.SetGlobalEnrichHandler(nil)
	observability.SetGlobalLogLeveler(logging.NewLevelMap(slog.LevelDebug))

	h := logging.NewDefaultHandler()
	logging.SetGlobalEnrichHandler(h)
	observability.SetGlobalLogLeveler(logging.NewLevelMap(slog.LevelError))
	require.NotNil(t, h.Leveler())
	assert.Equal(t, slog.LevelError, h.Leveler().DefaultLevel())
}

func TestInitWiring_SetGlobalLogSink(t *testing.T) {
	t.Cleanup(func() { logging.SetGlobalEnrichHandler(nil) })

	logging.SetGlobalEnrichHandler(nil)
	observability.SetGlobalLogSink(&captureSink{})

	h := logging.NewDefaultHandler()
	logging.SetGlobalEnrichHandler(h)
	sink := &captureSink{name: "wired"}
	observability.SetGlobalLogSink(sink)
	assert.Equal(t, sink, h.Sink())
}
