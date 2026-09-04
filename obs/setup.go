package obs

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup wires the providers and the exporter, for an application that has not
// already configured OpenTelemetry itself.
//
// # Why there is no collector in this repository
//
// Telemetry has to go somewhere, and the OpenTelemetry Collector is the
// something: a standalone binary that receives, batches, redacts and forwards.
// It is not built here and should not be. It is a mature, security sensitive,
// high throughput piece of infrastructure with a large community behind it,
// and writing our own would be the same mistake as shipping an exchange client
// inside recon.
//
// It is also already modular, which is usually the reason people reach for
// writing one. The upstream builder (ocb) takes a YAML manifest naming exactly
// which receivers, processors and exporters you want and compiles a binary
// with those and nothing else. deploy/otel-collector.yaml is a configuration
// for it, not a reimplementation of it.
//
// And it is optional. The SDK speaks OTLP directly to any backend that accepts
// it, which is enough for development and for small deployments. A collector
// earns its place in production by letting the application hand data off
// quickly and by being the one place retries, batching and redaction are
// configured.
//
// # What decides where the data goes
//
// Nothing here. Selection is delegated to the standard OpenTelemetry
// environment variables, so giro has no configuration surface of its own to
// learn, and pointing it somewhere else is a deployment change rather than a
// code change:
//
//	OTEL_METRICS_EXPORTER      otlp (default), prometheus, console, none
//	OTEL_TRACES_EXPORTER       otlp (default), console, none
//	OTEL_EXPORTER_OTLP_ENDPOINT  where the collector is
//	OTEL_EXPORTER_OTLP_PROTOCOL  grpc or http/protobuf
//
// That is the modular part, and it comes free: "none" is a real setting, so
// telemetry can be switched off in an environment without touching the wiring
// that would otherwise rot while it was disabled.
//
// Returns the Observer and a shutdown function. Call the shutdown function --
// metrics are batched, and a process that exits without flushing loses the
// interval it was in the middle of, which is reliably the interesting one.
func Setup(ctx context.Context, service string, opts Options) (*Observer, func(context.Context) error, error) {
	// Schemaless on purpose. resource.Default carries whatever semconv version
	// the SDK was built against, and Merge refuses to combine two resources
	// whose schema URLs differ -- so pinning a version here means this fails
	// the next time the SDK is upgraded, at startup, in whatever environment
	// upgraded first. A resource with no schema URL merges with any of them.
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, nil, fmt.Errorf("build resource: %w", err)
	}

	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)

	spans, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		// the meter provider is already running, so it has to be stopped
		// before returning or the caller leaks a goroutine on an error path
		return nil, nil, errors.Join(
			fmt.Errorf("span exporter: %w", err),
			mp.Shutdown(ctx),
		)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(spans),
	)

	// set globally so anything else in the process -- an HTTP handler, a
	// database driver -- lands in the same trace as the commit it caused
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)

	if opts.Meter == nil {
		opts.Meter = mp
	}
	if opts.Tracer == nil {
		opts.Tracer = tp
	}
	observer, err := New(opts)
	if err != nil {
		return nil, nil, errors.Join(err, mp.Shutdown(ctx), tp.Shutdown(ctx))
	}

	shutdown := func(ctx context.Context) error {
		// both, whatever the first one does, so a failing exporter cannot
		// leave the other provider running
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return observer, shutdown, nil
}
