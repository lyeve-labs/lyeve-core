package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DBTracer wraps database operations with OpenTelemetry spans.
// Use this from the internal/db package to instrument QueryRow, Query,
// Exec, and Begin without importing OTel SDK directly into DB code.
//
// When the global TracerProvider is the noop default (tracing disabled),
// every method is a zero-cost pass-through.
type DBTracer struct {
	tracer trace.Tracer
}

// NewDBTracer creates a DBTracer that emits spans named "{operation}: {summary}".
func NewDBTracer(serviceName string) *DBTracer {
	return &DBTracer{
		tracer: otel.Tracer(serviceName, trace.WithInstrumentationVersion("v1")),
	}
}

// Span starts a database span. Caller must defer span.End().
// summary is a short SQL description, not the full query (avoids leaking
// sensitive data to the trace collector).
func (t *DBTracer) Span(ctx context.Context, operation, summary string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "db."+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.operation", operation),
			attribute.String("db.statement", summary),
			attribute.String("db.system", "sql"),
		),
	)
}

// RecordError sets the error status on the span and records the error message.
// Returns true if the span was recording (non-zero when tracing is enabled).
func RecordError(span trace.Span, err error) bool {
	if err != nil && span.IsRecording() {
		span.SetStatus(codes.Error, err.Error())
		return true
	}
	return false
}

// TruncateSQL trims the SQL string to maxLen chars for safe span recording.
// Full SQL is logged. Only a prefix goes to traces.
func TruncateSQL(sql string, maxLen int) string {
	if len(sql) > maxLen {
		return sql[:maxLen] + "..."
	}
	return sql
}

// SQLSummary extracts a short (first 80 chars) summary of the SQL for span naming.
func SQLSummary(sql string) string {
	return TruncateSQL(sql, 80)
}

// DBQuerySpan is a convenience helper that opens+ends a span around a database
// query, recording the query summary and errors. For use by plugin stores.
//
// Usage:
//
//	func (s *MyStore) GetByID(ctx context.Context, id string) (*Item, error) {
//	    ctx, span := tracing.DBQuerySpan(ctx, "GetByID", "SELECT...")
//	    defer span.End()
//	    // ... do query work ...
//	    if err != nil { span.RecordError(err) }
//	    return item, err
//	}
func DBQuerySpan(ctx context.Context, tracer *DBTracer, operation, sql string) (context.Context, trace.Span) {
	if tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return tracer.Span(ctx, operation, SQLSummary(sql))
}
