// Package lua implements limits.Limiter over the Lua rate limiter in pkg/ratelimit: each atomicity group of a request's counters is reserved by one script whose keys share the group's hash tag, and every group's script goes out in one pipelined round trip. A refusal or store failure in any group releases the groups that reserved, so a refused request holds nothing; when several refuse, the first in order (caller groups, then upstream groups, each in list order) is the one reported. Budgets are spend meters in the same scripts: a hard budget is checked at admission (refused once spent ≥ cap, retry-after at the next period start), an alert budget is not, and both are charged the real cost at Settle.
//
// Scope: grouping, ordering and rollback across groups, mapping counters to limiter rules and refusals to counters, settlement, and reporting a budget's first crossing per period. Out of scope: which counters a request touches (app/plan), key and window arithmetic (pkg/ratelimit), and batching settles across requests.
//
// Expected kv ops per request:
//   - Admit: one round trip: a pipeline of one script per group that checks something, or a single script call when the request has one group; a group of alert budgets alone queues none, so a request of alert budgets only makes no call. A refusal or failure in any group while others reserved adds one batched round trip, on the post-flight store, releasing them.
//   - PrepareAdmit: the same scripts queued on the caller's kv.Batch, no round trip of its own; a refusal adds the same release.
//   - Settle: one batched round trip committing the groups that settle something (a concurrency lease, a token meter, a spend charge); none when nothing does.
//   - Release: one batched round trip.
//   - Peek (admin reads, off the request path): one batched round trip of read-only scripts, one per group.
//
// The admission estimate is unused: token meters are checked against what earlier requests used and charged at Settle, and budgets reserve nothing. A rollback or Release refunds every strategy's request count, so a refused or unsent request consumes no quota.
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

var (
	_ limits.Limiter       = (*Limiter)(nil)
	_ limits.BatchAdmitter = (*Limiter)(nil)
)

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

// Admit reserves every group of cs in one round trip. With a caller group the call sheds while the store's connection queue is full, as inbound admission does; upstream groups alone wait for a connection.
func (l *Limiter) Admit(ctx context.Context, cs []limits.Counter, _ limits.Usage) (limits.Reservation, error) {
	if len(cs) == 0 {
		return noCounters{}, nil
	}
	groups, err := groupCounters(cs)
	if err != nil {
		return nil, err
	}
	if len(groups) == 1 {
		return l.admitGroup(ctx, cs, &groups[0])
	}
	b := l.admit.NewBatch(len(groups))
	p, err := l.prepare(b, cs, groups)
	if err != nil {
		return nil, err
	}
	b.Exec(ctx)
	return p.Finish(ctx)
}

// admitGroup reserves a request's only group with one script call, sparing the batch.
func (l *Limiter) admitGroup(ctx context.Context, cs []limits.Counter, g *group) (limits.Reservation, error) {
	reserve := l.admit.ReserveOrShed
	if g.upstream {
		reserve = l.admit.Reserve
	}
	r, err := reserve(ctx, g.name, g.rules)
	if err != nil {
		return nil, refusal(cs, g.name, err)
	}
	return &reservation{lim: l, counters: cs, held: []*pkgratelimit.Reservation{r}}, nil
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
