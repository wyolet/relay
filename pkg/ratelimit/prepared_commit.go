package ratelimit

import (
	"fmt"

	"github.com/wyolet/relay/pkg/kv"
)

// PreparedCommit is a Commit queued on a kv batch; Finish reads its outcome
// once the batch has run.
type PreparedCommit struct {
	l      *Limiter
	res    *Reservation
	obs    Observations
	result *kv.BatchResult
}

// PrepareCommit queues the commit script for res on b, built exactly as
// Commit builds it. It returns nil when there is nothing to commit, as Commit
// would make no call.
func (l *Limiter) PrepareCommit(b *kv.Batch, res *Reservation, obs Observations) (*PreparedCommit, error) {
	if !res.needsCommit(obs) {
		return nil, nil
	}
	call, err := l.buildCommitCall(res, obs, l.clock())
	if err != nil {
		return nil, err
	}
	return &PreparedCommit{l: l, res: res, obs: obs, result: b.Script(call)}, nil
}

// Finish returns the commit script's failure, as Commit would.
func (p *PreparedCommit) Finish() error {
	if p.result.Err != nil {
		return fmt.Errorf("limit: commit script: %w", p.result.Err)
	}
	p.l.logCommit(p.res, p.obs, p.result.Value)
	return nil
}
