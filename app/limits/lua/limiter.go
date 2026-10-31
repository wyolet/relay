// Package lua implements limits.Limiter over the Lua rate limiter in pkg/ratelimit: each atomicity group of a request's counters is reserved by one script whose keys share the group's hash tag, caller groups first and then upstream groups in list order. A refusal or store failure in a later group releases the groups already reserved, so a refused request holds nothing. Budgets are spend meters in the same scripts: a hard budget is checked at admission (refused once spent ≥ cap, retry-after at the next period start), an alert budget is not, and both are charged the real cost at Settle.
//
// Scope: grouping, ordering and rollback across groups, mapping counters to limiter rules and refusals to counters, settlement, and reporting a budget's first crossing per period. Out of scope: which counters a request touches (app/plan), key and window arithmetic (pkg/ratelimit), and batching settles across requests.
//
// Expected kv ops per request:
//   - Admit: one script call per group that checks something, sequentially; a group of alert budgets alone makes none. A refusal or failure after a group reserved adds one batched round trip releasing them.
//   - Settle: one batched round trip committing the groups that settle something (a concurrency lease, a token meter, a spend charge); none when nothing does.
//   - Release: one batched round trip.
//
// The admission estimate is unused: token meters are checked against what earlier requests used and charged at Settle, and budgets reserve nothing. As in pkg/ratelimit, a release refunds token-bucket, leaky-bucket and session-window charges but not fixed- or sliding-window request counts.
package lua

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/app/limits"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// releaseTimeout bounds the writes after admission (rollback, Settle, Release), which ignore the caller's cancellation.
const releaseTimeout = 2 * time.Second

// Limiter admits requests on one pkg/ratelimit.Limiter and settles them on another, which may be the same.
type Limiter struct {
	admit *pkgratelimit.Limiter
	// post runs every write after admission, so a settle is not dropped while admission holds every connection of a shared pool.
	post    *pkgratelimit.Limiter
	crossed func(*limits.Counter)
}

var _ limits.Limiter = (*Limiter)(nil)

// New returns a Limiter that admits and settles on lim.
func New(lim *pkgratelimit.Limiter) *Limiter { return &Limiter{admit: lim, post: lim} }

// WithPostFlight runs rollbacks, Settle and Release on post instead. Call before serving.
func (l *Limiter) WithPostFlight(post *pkgratelimit.Limiter) *Limiter {
	l.post = post
	return l
}

// OnBudgetCrossed calls fn from Settle with each budget counter whose spend that settle took to its cap, once per budget and period. fn must not block. Call before serving.
func (l *Limiter) OnBudgetCrossed(fn func(*limits.Counter)) *Limiter {
	l.crossed = fn
	return l
}

// Admit reserves every group of cs in order. Caller groups shed while the store's connection queue is full, as inbound admission does; upstream groups wait for a connection.
func (l *Limiter) Admit(ctx context.Context, cs []limits.Counter, _ limits.Usage) (limits.Reservation, error) {
	if len(cs) == 0 {
		return noCounters{}, nil
	}
	groups, err := groupCounters(cs)
	if err != nil {
		return nil, err
	}
	res := &reservation{lim: l, counters: cs, held: make([]*pkgratelimit.Reservation, 0, len(groups))}
	for _, g := range groups {
		reserve := l.admit.ReserveOrShed
		if g.upstream {
			reserve = l.admit.Reserve
		}
		r, err := reserve(ctx, g.name, g.rules)
		if err != nil {
			// Released before the refusal returns, so a caller retrying at once does not meet its own hold.
			res.commit(ctx, pkgratelimit.Observations{Cancelled: true})
			return nil, refusal(cs, g.name, err)
		}
		res.held = append(res.held, r)
	}
	return res, nil
}

// refusal names the counter a group's reserve refused. Any other failure passes through: a limit that cannot be checked refuses the request.
func refusal(cs []limits.Counter, group string, err error) error {
	var ex *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &ex) {
		return err
	}
	for i := range cs {
		if cs[i].Group == group && ruleKey(&cs[i]) == ex.Rule.Key {
			return &limits.Refused{Counter: &cs[i], RetryAfter: ex.RetryAfter}
		}
	}
	return fmt.Errorf("limits: refused by unknown counter %s in group %s: %w", ex.Rule.Key, group, err)
}

// noCounters is the reservation of a request with nothing to count.
type noCounters struct{}

func (noCounters) Settle(limits.Usage) {}
func (noCounters) Release()            {}
