package usage

import (
	"context"
	"testing"
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
