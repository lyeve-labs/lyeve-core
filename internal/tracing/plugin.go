package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PluginSpan starts an OpenTelemetry span for a plugin operation.
// Plugins should use this to wrap their handler logic:
//
//	func (h *MyHandler) Create(w http.ResponseWriter, r *http.Request) {
//	    ctx, span := tracing.PluginSpan(r.Context(), "my-plugin", "Create", r.Method+" "+r.URL.Path)
//	    defer span.End()
//	    // ... handler logic ...
//	    if err != nil { tracing.RecordError(span, err) }
//	}
//
// When the global TracerProvider is the noop default (tracing disabled),
// PluginSpan returns a noop span and the original context unchanged.
func PluginSpan(ctx context.Context, pluginName, operation, route string) (context.Context, trace.Span) {
	tracer := otel.Tracer("cms-plugin-"+pluginName, trace.WithInstrumentationVersion("v1"))
	ctx, span := tracer.Start(ctx, pluginName+"."+operation,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("plugin.name", pluginName),
			attribute.String("plugin.operation", operation),
			attribute.String("http.route", route),
		),
	)
	return ctx, span
}
