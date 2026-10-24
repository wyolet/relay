package ratelimit

import (
	"context"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

// ReserveOrShed is Reserve for admission: while the store's connection wait queue is at its bound it returns ErrUnavailable at once with the script unsent, where Reserve would wait out the pool timeout to fail the same way.
func (l *Limiter) ReserveOrShed(ctx context.Context, scope string, rules []Rule) (*Reservation, error) {
	return l.reserve(ctx, l.shedRunner, scope, rules)
}

// shedScripter runs RunScript as the store's RunScriptOrShed.
type shedScripter struct{ kv.PoolShedder }

func (s shedScripter) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	return s.RunScriptOrShed(ctx, name, script, keys, args...)
}

// reserveShed counts reserves refused unsent for a full connection wait queue; resolved once, since under overload the refusal is the hot path.
var reserveShed = metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVShed)
