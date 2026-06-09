// Package logging provides centralized structured logging with context-aware
// enrichment, log level management per tenant/plugin, and log sink backends.
//
// It extends the standard library's log/slog with:
//   - Context-propagation of trace-id and span-id from OpenTelemetry spans
//   - Per-tenant and per-plugin log level overrides
//   - A custom slog.Handler that enriches all log records with request metadata
//   - Pluggable log sinks through observability.LogSink
//   - Log search and retention, which a plugin can supply
package logging

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/trace"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Context keys

type contextKey string

const (
	ctxKeyLogger    contextKey = "logger"
	ctxKeyPlugin    contextKey = "plugin"
	ctxKeyLogLevel  contextKey = "log_level"
	ctxKeyRequestID contextKey = "request_id"
)

// Context getters and setters

// WithLogger returns a copy of ctx with l stored.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// Logger retrieves the enriched logger from ctx, falling back to slog.Default().
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithPlugin returns a copy of ctx with the plugin name stored.
func WithPlugin(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKeyPlugin, name)
}

// PluginFromCtx extracts the plugin name from ctx. Returns "" if not set.
func PluginFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyPlugin).(string); ok {
		return v
	}
	return ""
}

// WithRequestID stores a request ID in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFromCtx extracts the request ID from ctx.
func RequestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// Log enrichment helpers

// EnrichAttrs returns log attributes extracted from the context, including
// trace-id, span-id, tenant-id, request-id, and plugin name. These are
// attached to every log record by the EnrichHandler.
func EnrichAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr

	span := trace.SpanFromContext(ctx)
	if sc := span.SpanContext(); sc.IsValid() {
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()))
	}

	if reqID := middleware.GetReqID(ctx); reqID != "" {
		attrs = append(attrs, slog.String("request_id", reqID))
	}

	if tenantID := core.TenantIDFromCtx(ctx); tenantID != "" && tenantID != core.TenantIDFromCtx(context.Background()) {
		attrs = append(attrs, slog.String("tenant_id", tenantID))
	}

	if plugin := PluginFromCtx(ctx); plugin != "" {
		attrs = append(attrs, slog.String("plugin", plugin))
	}

	return attrs
}

// TraceID extracts the trace ID from context for correlation.
func TraceID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if sc := span.SpanContext(); sc.IsValid() {
		return sc.TraceID().String()
	}
	// Fall back to request ID for untraced requests.
	return RequestIDFromCtx(ctx)
}

// SpanID extracts the span ID from context for correlation.
func SpanID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if sc := span.SpanContext(); sc.IsValid() {
		return sc.SpanID().String()
	}
	return ""
}

// Level overrides

// WithLevel returns a copy of ctx with a log level override for this request.
func WithLevel(ctx context.Context, level slog.Level) context.Context {
	return context.WithValue(ctx, ctxKeyLogLevel, level)
}

// LevelFromCtx returns the log level override from ctx if set.
func LevelFromCtx(ctx context.Context) slog.Level {
	if v, ok := ctx.Value(ctxKeyLogLevel).(slog.Level); ok {
		return v
	}
	return slog.LevelInfo // default when unset
}
