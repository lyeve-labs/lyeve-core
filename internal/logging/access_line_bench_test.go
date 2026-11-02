package logging_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/stretchr/testify/assert"
)

type nopSink struct{}

func (nopSink) Write(observability.LogEntry) {}
func (nopSink) Close() error                 { return nil }
func (nopSink) Name() string                 { return "nop" }

type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }

func accessLineContext() context.Context {
	ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-1")
	return core.WithTenantID(ctx, "acme")
}

// A line from a logger with no derived fields and no group has nothing to
// merge, so it skips the merge and allocates what it did before derived
// fields reached the stored entry. Every request writes such a line. The
// console handler is a no-op here, so the count is the enrich handler's own.
func TestEnrichHandler_PlainLineSkipsTheDerivedMerge(t *testing.T) {
	h := logging.NewEnrichHandler(nopHandler{}, nil)
	h.SetSink(nopSink{})
	ctx := accessLineContext()
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "request", 0)
	rec.AddAttrs(
		slog.String("method", "GET"),
		slog.String("path", "/api/admin/logging/levels"),
		slog.Int("status", 200),
		slog.Int("bytes", 51),
		slog.Int64("duration_ms", 1),
		slog.String("request_id", "req-1"),
	)

	got := testing.AllocsPerRun(500, func() { _ = h.Handle(ctx, rec) })
	assert.LessOrEqual(t, got, float64(27+raceDetectorAllocs))
}

// BenchmarkEnrichHandler_AccessLine logs the line every request writes,
// through a logger with no derived fields and no group, to a sink and the
// console.
func BenchmarkEnrichHandler_AccessLine(b *testing.B) {
	h := logging.NewEnrichHandler(slog.NewJSONHandler(io.Discard, nil), nil)
	h.SetSink(nopSink{})
	logger := h.Logger()
	ctx := accessLineContext()

	b.ReportAllocs()
	for b.Loop() {
		logger.InfoContext(ctx, "request",
			"method", "GET",
			"path", "/api/admin/logging/levels",
			"status", 200,
			"bytes", 51,
			"duration_ms", int64(1),
			"request_id", "req-1",
		)
	}
}
