package limit

// storeFactory produces a state.Store for parameterized tests.
// The concrete type is defined per build tag (mem vs integration).

import (
	"io"
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/state"
)

type limiterFactory func() *Limiter

func newLimiterFromStore(s state.Store, now *time.Time) *Limiter {
	clock := func() time.Time { return *now }
	return New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), clock)
}

func memStoreFactory() state.Store {
	return state.New()
}

func makeRuleWith(meter configstore.Meter, amount int64, window time.Duration, name string) configstore.ResolvedRule {
	return configstore.ResolvedRule{
		ParentKind: configstore.KindRoute,
		ParentName: "test-route",
		Meter:      meter,
		RateLimit: &configstore.RateLimit{
			Metadata: configstore.Metadata{Name: name},
			Spec: configstore.RateLimitSpec{
				Strategy: configstore.StrategySlidingWindow,
				Window:   window,
				Amount:   amount,
			},
		},
	}
}
