// Package tracing wires up OpenTelemetry tracing for cozytempl.
//
// The package keeps the OTel boilerplate out of main.go and exposes a
// single Init function that returns a shutdown handle. Every exported
// span in the app goes through otel.Tracer("cozytempl") so a single
// Instrumentation Scope covers the whole binary.
//
// The exporter is configured via the standard OTEL_* environment
// variables (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_PROTOCOL,
// etc.) so the operator can wire up Tempo, Jaeger, Honeycomb, SigNoz
// or anything else without a code change.
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is unset, Init returns a no-op
// shutdown function and the global TracerProvider stays at its
// zero-cost default. Tracing is always opt-in — operators who don't
// have a collector don't pay for spans they won't read.
package tracing

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// TracerName is the OpenTelemetry Instrumentation Scope name used
// by every tracer cozytempl creates. Keep stable so span filters
// in the backend don't break on upgrade.
const TracerName = "cozytempl"

// serviceNameEnv overrides the span resource's service.name.
// Defaults to "cozytempl" when unset, so multiple deployments in
// the same cluster can distinguish their spans by setting this.
const serviceNameEnv = "OTEL_SERVICE_NAME"

// shutdownTimeout bounds how long Shutdown blocks while flushing
// in-flight spans on process termination. Too short = lost spans;
// too long = SIGKILL before the pod can exit. Five seconds is the
// usual sweet spot.
const shutdownTimeout = 5 * time.Second

// Shutdown is the cleanup handle returned by Init. Callers defer
// this after Init so any buffered spans get flushed on graceful
// shutdown. The returned function is safe to call even when Init
// decided not to configure an exporter — it's a no-op in that case.
type Shutdown func(context.Context) error

// Init configures the global OpenTelemetry TracerProvider.
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is empty, tracing is disabled and
// Init returns a no-op Shutdown. Otherwise it builds an OTLP
// exporter (gRPC by default, HTTP if OTEL_EXPORTER_OTLP_PROTOCOL is
// set to "http/protobuf") and wires it into a batch span processor.
//
// The context is only used for the exporter handshake; the
// returned Shutdown takes its own context so the caller controls
// the flush deadline.
//
// The W3C TraceContext and Baggage propagators are installed so
// upstream trace IDs flow through cozytempl and into the k8s
// API-client calls (once we instrument them).
func Init(ctx context.Context) (Shutdown, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// Tracing is opt-in. Operators without a collector should
		// not be paying for a span pipeline they never read.
		noop := func(context.Context) error { return nil }

		return noop, nil
	}

	exporter, err := buildExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("building OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(envOrDefault(serviceNameEnv, "cozytempl")),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
	)
	if err != nil {
		return nil, fmt.Errorf("building resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()

		shutdownErr := provider.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			return fmt.Errorf("shutting down tracer provider: %w", shutdownErr)
		}

		return nil
	}, nil
}

// buildExporter picks between gRPC (default) and HTTP OTLP based on
// OTEL_EXPORTER_OTLP_PROTOCOL. "http/protobuf" selects HTTP,
// anything else (including empty) uses gRPC.
func buildExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL") == "http/protobuf" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("http OTLP exporter: %w", err)
		}

		return exp, nil
	}

	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("grpc OTLP exporter: %w", err)
	}

	return exp, nil
}

func envOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return fallback
}
