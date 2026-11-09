package lua

import (
	"context"

	"github.com/wyolet/relay/app/limits"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Peek reads cs from the store in one batched round trip, a read-only script per group, through the keys Admit reserves.
func (l *Limiter) Peek(ctx context.Context, cs []limits.Counter) ([]limits.Reading, error) {
	out := make([]limits.Reading, len(cs))
	if len(cs) == 0 {
		return out, nil
	}
	var (
		scopes []pkgratelimit.PeekScope
		at     [][]int // per scope, the index in cs of each rule
	)
	byGroup := map[string]int{}
	for i := range cs {
		c := &cs[i]
		r, err := toRule(c)
		if err != nil {
			return nil, err
		}
		out[i].Amount = r.Amount
		g, ok := byGroup[c.Group]
		if !ok {
			g = len(scopes)
			byGroup[c.Group] = g
			scopes = append(scopes, pkgratelimit.PeekScope{Scope: c.Group})
			at = append(at, nil)
		}
		scopes[g].Rules = append(scopes[g].Rules, r)
		at[g] = append(at[g], i)
	}
	counts, err := l.admit.Peek(ctx, scopes)
	if err != nil {
		return nil, err
	}
	for g, idx := range at {
		for j, i := range idx {
			out[i].Used = counts[g][j]
		}
	}
	return out, nil
}
