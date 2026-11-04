package lua

import (
	"context"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// PreparedAdmit is an Admit queued on a kv batch: every group's reserve rides the batch, so the groups cost one round trip together and the caller may queue writes of its own ahead of them.
type PreparedAdmit struct {
	lim      *Limiter
	counters []limits.Counter
	names    []string
	reserves []*pkgratelimit.PreparedReserve
}

// PrepareAdmit queues the reserve of every group of cs on b, as Admit does on a batch of its own; the caller runs b, then calls Finish. A group is reserved even when an earlier one refuses, and Finish releases it. When cs holds a caller group, b sheds while the store's connection queue is full.
func (l *Limiter) PrepareAdmit(b *kv.Batch, cs []limits.Counter) (limits.PreparedAdmit, error) {
	groups, err := groupCounters(cs)
	if err != nil {
		return nil, err
	}
	return l.prepare(b, cs, groups)
}

func (l *Limiter) prepare(b *kv.Batch, cs []limits.Counter, groups []group) (*PreparedAdmit, error) {
	var err error
	p := &PreparedAdmit{lim: l, counters: cs, names: make([]string, len(groups)), reserves: make([]*pkgratelimit.PreparedReserve, len(groups))}
	for i, g := range groups {
		if !g.upstream {
			b.ShedWhenPoolBusy()
		}
		if p.reserves[i], err = l.admit.PrepareReserve(b, g.name, g.rules); err != nil {
			return nil, err
		}
		p.names[i] = g.name
	}
	return p, nil
}

// Finish returns what Admit would have once the batch has run: the reservation, or the refusal of the first group in order that refused, with every other group released.
func (p *PreparedAdmit) Finish(ctx context.Context) (limits.Reservation, error) {
	if len(p.counters) == 0 {
		return noCounters{}, nil
	}
	res := &reservation{lim: p.lim, counters: p.counters, held: make([]*pkgratelimit.Reservation, 0, len(p.reserves))}
	var (
		failed error
		group  string
	)
	for i, pr := range p.reserves {
		r, err := pr.Finish(ctx)
		if err != nil {
			if failed == nil {
				failed, group = err, p.names[i]
			}
			continue
		}
		res.held = append(res.held, r)
	}
	if failed != nil {
		res.commit(ctx, pkgratelimit.Observations{Cancelled: true})
		return nil, refusal(p.counters, group, failed)
	}
	return res, nil
}

// Rule renders c as the limiter rule that counts it, for a limiter that counts some counters outside these scripts in the same store keys.
func Rule(c *limits.Counter) (pkgratelimit.Rule, error) { return toRule(c) }
