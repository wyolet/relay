package lua

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/settle"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

var budgetCrossings = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "limits",
	Name:      "budget_crossings_total",
	Help:      "Budgets whose spend reached the cap in the current period, by rung and mode.",
}, []string{"rung", "mode"})

func init() { metrics.Register(budgetCrossings) }

// reservation holds one pkg/ratelimit reservation per group Admit reserved.
type reservation struct {
	lim      *Limiter
	counters []limits.Counter
	held     []*pkgratelimit.Reservation
}

// Settle commits every group with the real tokens and cost: token meters are trued up, concurrency leases freed, budgets charged.
func (r *reservation) Settle(actual limits.Usage) {
	r.commit(context.Background(), pkgratelimit.Observations{Tokens: actual.Tokens, CostNanos: actual.CostNanos})
}

// PrepareSettle queues Settle's commits on b, a batch on the post-flight store.
func (r *reservation) PrepareSettle(b *kv.Batch, actual limits.Usage) limits.PreparedSettle {
	p := &preparedSettle{r: r, obs: pkgratelimit.Observations{Tokens: actual.Tokens, CostNanos: actual.CostNanos}}
	for _, h := range r.held {
		// A commit that cannot be built is dropped, as Settle drops it.
		if pc, err := r.lim.post.PrepareCommit(b, h, p.obs); err == nil && pc != nil {
			p.commits = append(p.commits, pc)
		}
	}
	return p
}

type preparedSettle struct {
	r       *reservation
	obs     pkgratelimit.Observations
	commits []*pkgratelimit.PreparedCommit
}

func (p *preparedSettle) TransportFailed() bool {
	for _, pc := range p.commits {
		if settle.TransportError(pc.Err()) {
			return true
		}
	}
	return false
}

func (p *preparedSettle) Finish(context.Context) {
	for _, pc := range p.commits {
		if pc.Finish() != nil {
			continue
		}
		for _, rule := range pc.Crossed() {
			if c := p.r.counter(rule.Key); c != nil {
				p.r.lim.reportCrossing(c)
			}
		}
	}
}

func (p *preparedSettle) Resend(ctx context.Context) { p.r.commit(ctx, p.obs) }

// Release commits every group as cancelled: leases freed, request counts refunded, nothing charged.
func (r *reservation) Release() {
	r.commit(context.Background(), pkgratelimit.Observations{Cancelled: true})
}

// commit runs on the post-flight limiter, bounded and immune to the caller's cancellation, and reports the budgets the charge took to their cap. A failed commit is counted by pkg/ratelimit and otherwise dropped: a lease left behind expires with its window.
func (r *reservation) commit(ctx context.Context, obs pkgratelimit.Observations) {
	if len(r.held) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	crossed, _ := r.lim.post.CommitAll(ctx, obs, r.held...)
	for _, rule := range crossed {
		if c := r.counter(rule.Key); c != nil {
			r.lim.reportCrossing(c)
		}
	}
}

func (r *reservation) counter(key string) *limits.Counter {
	for i := range r.counters {
		if r.counters[i].Kind == limits.KindBudget && ruleKey(&r.counters[i]) == key {
			return &r.counters[i]
		}
	}
	return nil
}

func (l *Limiter) reportCrossing(c *limits.Counter) {
	mode := budget.DefaultMode
	if c.Budget != nil && c.Budget.Spec.Mode != "" {
		mode = c.Budget.Spec.Mode
	}
	CountBudgetCrossing(c.Rung, mode)
	if l.crossed != nil {
		l.crossed(c)
	}
}

// CountBudgetCrossing counts a budget reaching its cap in relay_limits_budget_crossings_total, for a limiter that sees the crossing outside these scripts.
func CountBudgetCrossing(rung limits.Rung, mode budget.Mode) {
	budgetCrossings.WithLabelValues(string(rung), string(mode)).Inc()
}
