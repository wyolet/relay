//go:build integration

package policy

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func TestAdmitRedis(t *testing.T) {
	runAdmitSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

func TestSettleRedis(t *testing.T) {
	runSettleSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

func TestSettleQueueRedis(t *testing.T) {
	runSettleQueueSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

// BenchmarkSettleRedis is a served request's reserve and settle from many goroutines, each settle on its own round trip or through the queue.
func BenchmarkSettleRedis(b *testing.B) {
	for _, tc := range []struct {
		name  string
		queue SettleQueueConfig
	}{
		{"direct", SettleQueueConfig{}},
		{"queued", SettleQueueConfig{BatchSize: 64, MaxDelay: 2 * time.Millisecond, QueueSize: 4096}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			f := newQueueFixture(b, kvtest.NewRedis(b), 1<<40)
			f.svc.WithSettleQueue(tc.queue, discardLogger())
			b.Cleanup(f.svc.CloseSettleQueue)
			in := InboundInput{Policy: f.pol, TeamID: "team-1"}
			trips := f.store.RoundTrips()
			b.SetParallelism(16)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				for pb.Next() {
					res, err := f.svc.ReserveInbound(ctx, in)
					if err == nil {
						err = f.svc.Settle(ctx, res, nil, pkgratelimit.Observations{})
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.ReportMetric(float64(f.store.RoundTrips()-trips)/float64(b.N), "trips/op")
		})
	}
}

func TestRenameKeysRedis(t *testing.T) {
	runRenameSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
