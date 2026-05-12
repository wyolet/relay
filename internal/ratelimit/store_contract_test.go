package ratelimit

// storeFactory produces a kv.Store for parameterized tests.
// The concrete type is defined per build tag (mem vs integration).

import (
	"io"
	"log/slog"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/kv"
)

type limiterFactory func() *Limiter

func newLimiterFromStore(s kv.Store, now *time.Time) *Limiter {
	clock := func() time.Time { return *now }
	return New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), clock)
}

func memStoreFactory() kv.Store {
	return kv.NewMem()
}

func makeRuleWith(meter catalog.Meter, amount int64, window time.Duration, name string) catalog.ResolvedRule {
	return catalog.ResolvedRule{
		ParentKind: catalog.KindRoute,
		ParentName: "test-route",
		Meter:      meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: name},
			Spec: catalog.RateLimitSpec{
				Rules: []catalog.RateLimitRule{{Meter: string(meter), Amount: amount, Window: window, Strategy: catalog.StrategySlidingWindow}},
			},
		},
		Rule:     catalog.RateLimitRule{Meter: string(meter), Amount: amount, Window: window, Strategy: catalog.StrategySlidingWindow},
		Strategy: catalog.StrategySlidingWindow,
		Window:   window,
	}
}
