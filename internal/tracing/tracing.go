// Layer: observability — OpenTelemetry distributed tracing.
// Initialises a global TracerProvider backed by an OTLP/HTTP exporter.
// All services call tracing.Init() at startup. Request handlers call
// tracing.Start() to open a span; the returned context carries the span.
//
// Span names follow: "<service>.<operation>" (e.g. "api.create_sandbox").
//
// Phase 1: noop tracer when endpoint is not configured.
// Phase 2: real OTLP/HTTP export to a Grafana Tempo / Jaeger collector.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracer is the module-level tracer; set by Init.
var tracer trace.Tracer

// Init sets up a TracerProvider that exports spans to the given OTLP/HTTP endpoint.
// serviceName is the value that appears in trace UIs (e.g. "sandock-api").
// Call Init once at process startup before any calls to Start.
func Init(serviceName, endpoint string) error {
	ctx := context.Background()

	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(), // Phase 3: add mTLS for production.
	)
	if err != nil {
		return fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return fmt.Errorf("tracing: create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer(serviceName)
	return nil
}

// Start opens a new span named spanName, derived from ctx.
// The returned context carries the active span; pass it downstream.
// Call span.End() when the operation is complete.
//
// Example:
//
//	ctx, span := tracing.Start(ctx, "api.create_sandbox")
//	defer span.End()
func Start(ctx context.Context, spanName string) (context.Context, trace.Span) {
	if tracer == nil {
		// Tracing not initialised — return a noop span so callers need not nil-check.
		return otel.Tracer("noop").Start(ctx, spanName)
	}
	return tracer.Start(ctx, spanName)
}

// FromContext returns the span currently stored in ctx, or a noop span if none.
func FromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}
