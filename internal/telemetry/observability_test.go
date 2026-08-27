package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

func startTelemetry(t *testing.T, options observability.Options) (*observability.Telemetry, *bytes.Buffer) {
	t.Helper()

	logs := &bytes.Buffer{}
	options.LogOutput = logs
	if options.ServiceName == "" {
		options.ServiceName = "oc-control-plane-test"
	}

	telemetry, err := observability.Start(context.Background(), options)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := telemetry.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return telemetry, logs
}

// decodeOne parses the single JSON log line the test emitted.
func decodeOne(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	line := strings.TrimSpace(logs.String())
	if line == "" {
		t.Fatal("no log line was written")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", line)
	}
	return entry
}

func TestStart_RequiresItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := observability.Start(context.Background(), observability.Options{
		ServiceName: "x",
	}); err == nil {
		t.Error("a missing LogOutput must be refused")
	}
	if _, err := observability.Start(context.Background(), observability.Options{
		LogOutput: &bytes.Buffer{},
	}); err == nil {
		t.Error("a missing ServiceName must be refused")
	}
}

// A process with no collector configured must still start. Telemetry that refuses to
// assemble takes the service down with it, trading an observability gap for an outage.
func TestStart_SucceedsWithNoCollectorConfigured(t *testing.T) {
	t.Parallel()

	telemetry, _ := startTelemetry(t, observability.Options{OTLPEndpoint: ""})

	if telemetry.Tracer == nil {
		t.Error("a tracer must exist even with export disabled, so call sites stay identical")
	}
	if telemetry.MetricsHandler == nil {
		t.Error("metrics must be served regardless of trace export")
	}
}

func TestLoggerFor_AlwaysCarriesTheRequestIdentifier(t *testing.T) {
	t.Parallel()

	telemetry, logs := startTelemetry(t, observability.Options{})
	observability.LoggerFor(context.Background(), telemetry.Logger, "request-abc").
		Info("something happened")

	entry := decodeOne(t, logs)
	if entry["request_id"] != "request-abc" {
		t.Errorf("request_id = %v", entry["request_id"])
	}
}

// The acceptance criterion an on-call engineer depends on: a log line emitted inside a
// sampled span carries that span's trace and span identifiers, so moving from a log line
// to its trace needs no timestamp matching.
func TestLoggerFor_CarriesTraceAndSpanIdentifiersInsideASampledSpan(t *testing.T) {
	t.Parallel()

	telemetry, logs := startTelemetry(t, observability.Options{})

	// A local always-sampling provider: the process default never samples when no collector
	// is configured, which is correct in production and would make this assertion vacuous.
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "handle request")
	defer span.End()

	observability.LoggerFor(ctx, telemetry.Logger, "request-abc").Info("inside the span")

	entry := decodeOne(t, logs)
	spanContext := span.SpanContext()

	if entry["trace_id"] != spanContext.TraceID().String() {
		t.Errorf("trace_id = %v, want %s", entry["trace_id"], spanContext.TraceID())
	}
	if entry["span_id"] != spanContext.SpanID().String() {
		t.Errorf("span_id = %v, want %s", entry["span_id"], spanContext.SpanID())
	}
}

// Outside a span there is nothing to correlate to, and emitting an all-zero trace
// identifier would be worse than emitting none: it looks like a real value.
func TestLoggerFor_OmitsTraceIdentifiersOutsideASpan(t *testing.T) {
	t.Parallel()

	telemetry, logs := startTelemetry(t, observability.Options{})
	observability.LoggerFor(context.Background(), telemetry.Logger, "request-abc").
		Info("outside any span")

	entry := decodeOne(t, logs)
	if _, present := entry["trace_id"]; present {
		t.Error("trace_id must be absent outside a span, not zero-valued")
	}
	if _, present := entry["span_id"]; present {
		t.Error("span_id must be absent outside a span, not zero-valued")
	}
}

// Tenant identity belongs on a span. The named constructor exists so the rule has one home;
// this test pins the attribute key so a rename cannot silently move it onto a metric.
func TestOrganizationAttribute_IsASpanAttribute(t *testing.T) {
	t.Parallel()

	attribute := observability.OrganizationAttribute("org-a")
	if string(attribute.Key) != "opencluster.organization" {
		t.Errorf("key = %q", attribute.Key)
	}
	if attribute.Value.AsString() != "org-a" {
		t.Errorf("value = %q", attribute.Value.AsString())
	}
}

func TestShutdown_IsSafeAndReportsSuccess(t *testing.T) {
	t.Parallel()

	logs := &bytes.Buffer{}
	telemetry, err := observability.Start(context.Background(), observability.Options{
		ServiceName: "oc-control-plane-test",
		LogOutput:   logs,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := telemetry.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	if err := telemetry.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown must be safe to call twice: %v", err)
	}
}
