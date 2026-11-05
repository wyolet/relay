package clickhouse

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// benchEvent is a fully attributed, priced usage event, so the read-back
// decodes what a production segment holds.
func benchEvent(i int) usage.Event {
	id := func(prefix string) string { return fmt.Sprintf("019a%s-0000-7000-8000-%012d", prefix, i%500) }
	cost := int64(1234567)
	return usage.Event{
		RequestID: fmt.Sprintf("019a0000-0000-7000-8000-%012d", i), Source: "pipeline",
		Timestamp: time.Unix(1_700_000_000, int64(i)).UTC(), Status: 200, DurationMs: 812, Streamed: true,
		FinishReason: "stop", Attempts: 1,
		Upstream:     &sdkusage.UpstreamTiming{Start: 120, ResponseStart: 45_000, ResponseEnd: 800_000},
		RelayKeyHash: id("0001"), PolicyID: id("0002"), ModelID: id("0003"), RequestedModel: "frontier-large-4-5",
		HostID: id("0004"), HostKeyID: id("0005"), PoolID: id("0006"), AccountID: id("0007"),
		ProjectID: id("0008"), Project: "checkout", TeamID: id("0009"), Team: "platform",
		PrincipalKind: "serviceaccount", PrincipalID: id("0010"), Principal: "ci-runner",
		CredentialKind: "key", CredentialID: id("0011"), LaneAccountID: id("0012"),
		Tokens: sdkusage.Tokens{"input": 12000, "output": 640, "cache_read": 9000},
		Tags:   map[string]string{"env": "prod"},
		Model:  "frontier-large-4-5", Host: "vendor-api", Policy: "default", Provider: "vendor-a", RelayKey: "ci",
		CostNanos: &cost, CostBreakdown: map[string]int64{"tokens.input": 1000000, "tokens.output": 234567},
		Pricing: "vendor-api-frontier-large-4-5",
	}
}

// BenchmarkDrain flushes a backlog of full segments through FlushPending
// with a fake insert that waits insertWait, as a ClickHouse round trip does.
func BenchmarkDrain(b *testing.B) {
	const (
		segments = 16
		lines    = 10_000
	)
	template := b.TempDir()
	q, err := newSegmentQueue(template, lines, time.Hour, 1024, testLogger, nil)
	if err != nil {
		b.Fatal(err)
	}
	batch := make([]usage.Event, 0, 1000)
	for i := range segments * lines {
		batch = append(batch, benchEvent(i))
		if len(batch) == cap(batch) {
			if err := q.WriteBatch(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	q.StopBackground()
	files, err := filepath.Glob(filepath.Join(template, "segment-*.jsonl"))
	if err != nil || len(files) != segments {
		b.Fatalf("template holds %d segments (%v), want %d", len(files), err, segments)
	}

	for _, insertWait := range []time.Duration{0, 50 * time.Millisecond} {
		b.Run(fmt.Sprintf("insert=%v", insertWait), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Hard links: the drain unlinks its copies, the template stays.
				dir := b.TempDir()
				for _, f := range files {
					if err := os.Link(f, filepath.Join(dir, filepath.Base(f))); err != nil {
						b.Fatal(err)
					}
				}
				q, err := newSegmentQueue(dir, lines, time.Hour, 1024, testLogger, func([]usage.Event) error {
					time.Sleep(insertWait)
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				q.FlushPending()
				b.StopTimer()
				q.StopBackground()
				if left, _ := filepath.Glob(filepath.Join(dir, "segment-*.jsonl")); len(left) != 0 {
					b.Fatalf("%d segments left after the drain", len(left))
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(b.N*segments*lines)/b.Elapsed().Seconds(), "records/s")
		})
	}
}
