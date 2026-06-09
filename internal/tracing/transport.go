package tracing

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TracedTransport is an http.RoundTripper that injects W3C trace context
// (traceparent/tracestate) into every outbound request and creates a CLIENT
// span for each HTTP call.
//
// Plugins use this to propagate trace context to external services:
//
//	client := &http.Client{
//	    Transport: tracing.NewTracedTransport(http.DefaultTransport, "my-plugin"),
//	}
//	resp, err := client.Get("https://api.external.com/data")
//
// When the global TracerProvider is the noop default (tracing disabled),
// TracedTransport passes through to the wrapped transport with near-zero
// overhead.
type TracedTransport struct {
	base       http.RoundTripper
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// NewTracedTransport returns a TracedTransport wrapping base.
// When base is nil, http.DefaultTransport is used.
// serviceName is the tracer name for CLIENT spans (e.g. "cms-webhook").
func NewTracedTransport(base http.RoundTripper, serviceName string) *TracedTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &TracedTransport{
		base:       base,
		tracer:     otel.Tracer(serviceName, trace.WithInstrumentationVersion("v1")),
		propagator: otel.GetTextMapPropagator(),
	}
}

// RoundTrip implements http.RoundTripper.
func (t *TracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	spanName := "HTTP " + req.Method + " " + req.URL.Host + req.URL.Path
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
	)
	defer span.End()

	span.SetAttributes(
		attribute.String("http.request.method", req.Method),
		attribute.String("url.full", req.URL.String()),
		attribute.String("server.address", req.URL.Host),
	)

	t.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if resp != nil {
		span.SetAttributes(
			attribute.Int("http.response.status_code", resp.StatusCode),
		)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else if resp != nil && resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	return resp, err
}
