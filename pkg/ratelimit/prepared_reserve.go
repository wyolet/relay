package ratelimit

import (
	"context"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/reqid"
)

// PreparedReserve is a Reserve queued on a kv batch; Finish reads its outcome once the batch has run.
type PreparedReserve struct {
	l      *Limiter
	scope  string
	rules  []Rule
	token  string
	now    time.Time
	result *kv.BatchResult // nil when nothing was queued: Alert spend rules alone, which Reserve does not check
}

// NewBatch returns a kv batch on the limiter's store with room for size commands, for PrepareReserve.
func (l *Limiter) NewBatch(size int) *kv.Batch { return kv.NewBatch(l.store, size) }

// PrepareReserve queues the reserve script for rules on b. The caller runs b, then calls Finish: same script, arguments and outcome as Reserve.
func (l *Limiter) PrepareReserve(b *kv.Batch, scope string, rules []Rule) (*PreparedReserve, error) {
	token := reqid.Generate()
	if onlyAlerts(rules) {
		for _, r := range rules {
			if _, _, err := periodBounds(r.Period, time.Time{}); err != nil {
				return nil, err
			}
		}
		return &PreparedReserve{l: l, scope: scope, rules: rules, token: token}, nil
	}
	now := l.clock()
	call, err := l.buildReserveCall(scope, rules, token, now)
	if err != nil {
		return nil, err
	}
	return &PreparedReserve{l: l, scope: scope, rules: rules, token: token, now: now, result: b.Script(call)}, nil
}

// Finish returns what Reserve would have: the reservation, *ExceededError, ErrRevoked, or the script's failure.
func (p *PreparedReserve) Finish(ctx context.Context) (*Reservation, error) {
	if p.result == nil {
		return newReservation(p.scope, p.rules, p.token, time.Time{}), nil
	}
	if p.result.Err != nil {
		p.l.releaseUnanswered(p.scope, p.rules, p.token, p.result.Err)
		return nil, reserveFailed(p.result.Err)
	}
	return p.l.decodeReserve(ctx, p.scope, p.rules, p.token, p.now, p.result.Value)
}
