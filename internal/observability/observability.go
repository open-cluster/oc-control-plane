// Package observability assembles the process's telemetry: structured logs, traces, and
// metrics. It is present from the start rather than retrofitted, so the first production
// incident is diagnosed with data instead of with an instrumentation branch.
//
// Three deliberate choices, recorded here because each has an alternative that looks
// equivalent and is not:
//
//   - Metrics are instrumented through the OpenTelemetry API but exported for Prometheus
//     scrape. A scrape endpoint is what Kubernetes operators already collect; instrumenting
//     through OTel means an OTLP exporter can be added later without touching a call site.
//   - Organization identity belongs on spans, never on a metric label. At the stated scale
//     of five thousand organizations a tenant label is a cardinality failure in any
//     Prometheus-shaped backend; exemplars connect an aggregate metric to a trace instead.
//   - Logs stay on log/slog with trace and span identifiers injected as attributes. The
//     OpenTelemetry logs signal is the least mature of the three in Go, and correlation by
//     attribute already satisfies what an on-call engineer needs.
package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	prometheusexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// The semconv version MUST match the one resource.Default() carries, or resource.Merge
	// refuses the pair with a schema-URL conflict.
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// shutdownTimeout bounds flushing telemetry on the way out. A process that hangs
// exporting spans during a rolling deployment is worse than one that drops them.
const shutdownTimeout = 5 * time.Second

// Options configure the telemetry stack.
type Options struct {
	// ServiceName identifies this process in traces and metrics.
	ServiceName string
	// ServiceVersion is the build identity, so a trace can be attributed to an artefact.
	ServiceVersion string
	// OTLPEndpoint is the trace collector, host:port. Empty disables trace export and
	// installs a no-op tracer, which is the correct default for a process with no
	// collector configured.
	OTLPEndpoint string
	// LogOutput receives structured logs. Tests supply a buffer; production supplies stderr.
	LogOutput io.Writer
}

// Telemetry is the assembled stack. Shutdown flushes and releases it.
type Telemetry struct {
	Logger         *slog.Logger
	Tracer         trace.Tracer
	MetricsHandler http.Handler

	shutdowns    []func(context.Context) error
	shutdownOnce sync.Once
	shutdownErr  error
}

// Start assembles logging, tracing, and metrics. It never fails on a missing collector:
// telemetry that refuses to start takes the service with it, which trades an observability
// gap for an outage.
func Start(ctx context.Context, options Options) (*Telemetry, error) {
	if options.LogOutput == nil {
		return nil, errors.New("observability: LogOutput is required")
	}
	if options.ServiceName == "" {
		return nil, errors.New("observability: ServiceName is required")
	}

	logger := slog.New(slog.NewJSONHandler(options.LogOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))

	attributes, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(options.ServiceName),
		semconv.ServiceVersion(options.ServiceVersion),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: building the resource: %w", err)
	}

	telemetry := &Telemetry{Logger: logger}

	tracerProvider, err := traceProvider(ctx, options.OTLPEndpoint, attributes)
	if err != nil {
		return nil, err
	}
	otel.SetTracerProvider(tracerProvider)
	telemetry.Tracer = tracerProvider.Tracer(options.ServiceName)
	telemetry.shutdowns = append(telemetry.shutdowns, tracerProvider.Shutdown)

	registry := prometheus.NewRegistry()
	metricExporter, err := prometheusexporter.New(prometheusexporter.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("observability: building the metric exporter: %w", err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(attributes),
		sdkmetric.WithReader(metricExporter),
	)
	otel.SetMeterProvider(meterProvider)
	telemetry.shutdowns = append(telemetry.shutdowns, meterProvider.Shutdown)
	telemetry.MetricsHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	return telemetry, nil
}

// traceProvider builds the tracer provider. With no endpoint configured it returns a
// provider that samples nothing, so instrumented code paths stay identical whether or not
// a collector exists.
func traceProvider(
	ctx context.Context, endpoint string, attributes *resource.Resource,
) (*sdktrace.TracerProvider, error) {
	if endpoint == "" {
		return sdktrace.NewTracerProvider(
			sdktrace.WithResource(attributes),
			sdktrace.WithSampler(sdktrace.NeverSample()),
		), nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("observability: building the trace exporter: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(attributes),
		sdktrace.WithBatcher(exporter),
	), nil
}

// Shutdown flushes and releases telemetry. Failures are joined rather than short-circuited
// so one stuck exporter cannot hide another's error.
//
// It is idempotent and returns the first attempt's result on every call. The underlying
// providers refuse a second shutdown, and a caller that both defers Shutdown and calls it
// on an error path should not be punished for it.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	t.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()

		for _, shutdown := range t.shutdowns {
			t.shutdownErr = errors.Join(t.shutdownErr, shutdown(ctx))
		}
	})
	return t.shutdownErr
}

// LoggerFor returns a logger carrying the request's correlation attributes: the request
// identifier, and the trace and span identifiers when the context is sampled. This is what
// lets an on-call engineer move between one log line and its trace without matching on
// timestamps.
func LoggerFor(ctx context.Context, logger *slog.Logger, requestID string) *slog.Logger {
	attributes := []any{slog.String("request_id", requestID)}

	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		attributes = append(attributes,
			slog.String("trace_id", span.TraceID().String()),
			slog.String("span_id", span.SpanID().String()))
	}
	return logger.With(attributes...)
}

// OrganizationAttribute returns the span attribute carrying tenant identity. It exists as
// a named function so the rule has one place to live: this value belongs on a span and
// must never become a metric label.
func OrganizationAttribute(organization string) attribute.KeyValue {
	return attribute.String("opencluster.organization", organization)
}
