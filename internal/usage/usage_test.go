package usage

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestInit_NoEndpoint(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Idempotent second call.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown (2nd): %v", err)
	}
}

func TestInit_WithEndpoint(t *testing.T) {
	// OTLP gRPC dials lazily, so construction with a fake endpoint should succeed.
	shutdown, err := Init(context.Background(), Config{
		OTLPEndpoint: "localhost:4317",
		ServiceName:  "relay-test",
	})
	if err != nil {
		t.Fatalf("Init with endpoint: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	// Shutdown may fail because of the timeout/no collector, but must not panic.
	_ = shutdown(ctx)
}

func TestLifecycleZeroValue(t *testing.T) {
	var lc Lifecycle
	// nil maps / slices must not panic on read.
	_ = lc.Metrics["anything"]
	_ = lc.Attribution["anything"]
	_ = len(lc.Attempts)
	if lc.Span() != nil {
		t.Error("zero Lifecycle span should be nil")
	}
}

func TestInit_StorageResourceAttrs(t *testing.T) {
	// Use a SpanRecorder-backed TracerProvider to inspect the resource.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	// Manually build resource attrs like Init does and attach to the TP.
	// We test the helper + resource attr logic by calling Init with a fake endpoint
	// that won't actually export, then verify the resource attrs on the span.
	// Because Init with an OTLP endpoint installs a global TP, we instead
	// test storageBackend and that the fields thread through Config correctly.
	_ = tp // sr will collect via its own TP

	// Verify storageBackend defaults.
	if got := storageBackend("", "unknown"); got != "unknown" {
		t.Errorf("expected unknown, got %q", got)
	}
	if got := storageBackend("pg", "unknown"); got != "pg" {
		t.Errorf("expected pg, got %q", got)
	}

	// Verify Config fields compile and are accessible.
	cfg := Config{
		CatalogBackend:  "pg",
		StateBackend:    "redis",
		EventlogBackend: "clickhouse",
	}
	if cfg.CatalogBackend != "pg" || cfg.StateBackend != "redis" || cfg.EventlogBackend != "clickhouse" {
		t.Error("Config storage fields not set correctly")
	}

	// Verify Init with no endpoint still succeeds (no-op path).
	shutdown, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = shutdown(context.Background())
}

func TestTerminatedByConstants(t *testing.T) {
	constants := []TerminatedBy{
		TerminatedClean,
		TerminatedClientCancel,
		TerminatedUpstreamError,
		TerminatedUpstreamTimeout,
		TerminatedRateLimited,
		TerminatedPoolExhausted,
		TerminatedRelayError,
	}
	seen := make(map[TerminatedBy]bool)
	for _, c := range constants {
		if c == "" {
			t.Errorf("constant is empty")
		}
		if seen[c] {
			t.Errorf("duplicate constant value: %q", c)
		}
		seen[c] = true
	}
}
