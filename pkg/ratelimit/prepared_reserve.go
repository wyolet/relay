package ratelimit

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/reqid"
)

// PreparedReserve is a Reserve queued on a kv batch; Finish reads its outcome
// once the batch has run.
type PreparedReserve struct {
	l      *Limiter
	scope  string
	rules  []Rule
	token  string
	result *kv.BatchResult
}

// PrepareReserve queues the reserve script for rules on b. The caller runs b,
// then calls Finish: same script, arguments and outcome as Reserve.
func (l *Limiter) PrepareReserve(b *kv.Batch, scope string, rules []Rule) (*PreparedReserve, error) {
	token := reqid.Generate()
	call, err := l.buildReserveCall(scope, rules, token)
	if err != nil {
		return nil, err
	}
	return &PreparedReserve{l: l, scope: scope, rules: rules, token: token, result: b.Script(call)}, nil
}

// Finish returns what Reserve would have: the reservation, *ExceededError,
// ErrRevoked, or the script's failure.
func (p *PreparedReserve) Finish(ctx context.Context) (*Reservation, error) {
	if p.result.Err != nil {
		return nil, fmt.Errorf("limit: reserve script: %w", p.result.Err)
	}
	return p.l.decodeReserve(ctx, p.scope, p.rules, p.token, p.result.Value)
}
