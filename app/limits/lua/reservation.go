package lua

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
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

// Release commits every group as cancelled: leases freed, bucket charges refunded, nothing charged.
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
	budgetCrossings.WithLabelValues(string(c.Rung), string(mode)).Inc()
	if l.crossed != nil {
		l.crossed(c)
	}
}
