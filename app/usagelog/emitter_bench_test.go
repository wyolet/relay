package usagelog

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/wal"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// walWriteSink exposes only the per-event path of a WAL queue.
type walWriteSink struct{ q *wal.Queue[Event] }

func (s walWriteSink) Write(ev Event) error { return s.q.Write(ev) }

func benchEvent(i int) Event {
	cost := int64(1234567)
	return Event{
		RequestID:      fmt.Sprintf("019a0000-0000-7000-8000-%012d", i),
		Source:         "pipeline",
		Timestamp:      time.Now().UTC(),
		Status:         200,
		DurationMs:     812,
		Streamed:       true,
		FinishReason:   "stop",
		Attempts:       1,
		Upstream:       &sdkusage.UpstreamTiming{Start: 3, ResponseStart: 240, ResponseEnd: 805},
		RelayKeyHash:   "5f2b8c1e9a7d4e6f0b3c2a1d8e9f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
		PolicyID:       "019a0000-0000-7000-8000-000000000001",
		ModelID:        "019a0000-0000-7000-8000-000000000002",
		RequestedModel: "example-model-large",
		HostID:         "019a0000-0000-7000-8000-000000000003",
		HostKeyID:      "019a0000-0000-7000-8000-000000000004",
		ProjectID:      "019a0000-0000-7000-8000-000000000005",
		Project:        "default",
		PrincipalKind:  "service_account",
		PrincipalID:    "019a0000-0000-7000-8000-000000000006",
		Principal:      "ci-bot",
		CredentialKind: "key",
		CredentialID:   "019a0000-0000-7000-8000-000000000007",
		Tokens:         sdkusage.Tokens{"input": 1830, "output": 412, "cache_read": 1024},
		Extras:         map[string]string{"instance": "relay-0"},
		Tags:           map[string]string{"team": "platform", "env": "prod"},
		Model:          "example-model-large",
		Host:           "example-host",
		Policy:         "default",
		Provider:       "example",
		RelayKey:       "ci-key",
		CostNanos:      &cost,
		CostBreakdown:  map[string]int64{"input": 900000, "output": 334567},
		Pricing:        "example-model-large",
	}
}

// benchmarkEmitterWAL emits 10k events per iteration into an emitter whose
// sink is a WAL queue, and waits for the drain to write them all.
func benchmarkEmitterWAL(b *testing.B, sink func(*wal.Queue[Event]) Sink) {
	const events = 10_000
	discard := slog.New(slog.DiscardHandler)
	evs := make([]Event, events)
	for i := range evs {
		evs[i] = benchEvent(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		q, err := wal.Open(wal.Config{Dir: b.TempDir(), MaxLines: 10_000, FlushInterval: time.Hour, Kind: "bench", Logger: discard},
			func([]Event) error { return nil })
		if err != nil {
			b.Fatal(err)
		}
		e := NewEmitter(EmitterOptions{QueueSize: events, Logger: discard}, sink(q))
		b.StartTimer()
		for j := range evs {
			e.Emit(evs[j])
		}
		e.Close()
		b.StopTimer()
		if e.Dropped() != 0 {
			b.Fatalf("dropped %d events", e.Dropped())
		}
		q.StopBackground()
		b.StartTimer()
	}
	elapsed := b.Elapsed().Seconds()
	b.ReportMetric(float64(b.N*events)/elapsed, "events/s")
	b.ReportMetric(b.Elapsed().Seconds()*1e9/float64(b.N*events), "ns/event")
}

// walBatchSink adds the batch path, as the ClickHouse sink does.
type walBatchSink struct{ walWriteSink }

func (s walBatchSink) WriteBatch(evs []Event) error { return s.q.WriteBatch(evs) }

func BenchmarkEmitterWAL_PerEvent(b *testing.B) {
	benchmarkEmitterWAL(b, func(q *wal.Queue[Event]) Sink { return walWriteSink{q} })
}

func BenchmarkEmitterWAL_Batch(b *testing.B) {
	benchmarkEmitterWAL(b, func(q *wal.Queue[Event]) Sink { return walBatchSink{walWriteSink{q}} })
}
