// Package tracing provides OpenTelemetry distributed tracing for LyEve CMS.
//
// It initializes an OTLP/gRPC trace exporter, sets the global TracerProvider,
// and provides middleware for W3C trace context propagation through HTTP
// requests, plugin calls, and database queries.
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is not configured, tracing is a no-op:
// the global TracerProvider remains the SDK default (noop).
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Config carries tracing initialization parameters.
type Config struct {
	Endpoint       string // OTLP collector address (e.g. "localhost:4317"). Empty = disabled.
	ServiceName    string // Defaults to "lyeve-core".
	ServiceVersion string // Semver tag (e.g. "v1.2.3").
	InstanceID     string // Unique process identifier.
	Insecure       bool   // Disables TLS for local collectors.

	// SamplingRate is the global trace sampling rate (0.0-1.0). Default 1.0.
	SamplingRate float64

	// TenantSamplingRates maps tenant slugs to sampling rates (0.0-1.0).
	// Tenants not listed fall back to SamplingRate. Nil/empty means no
	// per-tenant override.
	TenantSamplingRates map[string]float64
}

// Init sets up the global OpenTelemetry TracerProvider backed by an
// OTLP/gRPC exporter. When cfg.Endpoint is empty, Init is a no-op.
//
// The returned ShutdownFunc must be called before process exit to
// flush pending spans.
func Init(ctx context.Context, cfg Config) (shutdown ShutdownFunc, err error) {
	if cfg.Endpoint == "" {
		slog.Info("tracing: OTLP endpoint not configured - tracing disabled")
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "lyeve-core"
	}

	exp, err := newOTLPExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("service.version", cfg.ServiceVersion),
			attribute.String("service.instance.id", cfg.InstanceID),
		),
		resource.WithHost(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create resource: %w", err)
	}

	// Build the sampler: TenantSampler -> ParentBased so children inherit
	// parent decisions. When no per-tenant rates are configured, the
	// TenantSampler just uses SamplingRate as a uniform rate.
	sampler := NewTenantSampler(cfg.TenantSamplingRates, cfg.SamplingRate)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("tracing: OTLP exporter initialized",
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"version", cfg.ServiceVersion,
	)

	return tp.Shutdown, nil
}

// ShutdownFunc flushes pending spans and closes the exporter.
type ShutdownFunc func(context.Context) error

// newOTLPExporter creates an OTLP/gRPC trace exporter.
func newOTLPExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithTimeout(10 * time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 1 * time.Second,
			MaxInterval:     30 * time.Second,
		}),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithDialOption(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
	}
	return otlptracegrpc.New(ctx, opts...)
}
