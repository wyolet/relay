package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
)

const (
	// unansweredReleaseTimeout bounds one release, which runs detached from the refused request.
	unansweredReleaseTimeout = 2 * time.Second
	// maxUnansweredReleases caps the releases in flight per Limiter; past it a lease is left to expire.
	maxUnansweredReleases = 64
)

var unansweredReleases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "ratelimit",
	Name:      "unanswered_reserve_releases_total",
	Help:      "Lease releases for reserves the store may have run without answering, by outcome (released, failed, skipped = too many in flight).",
}, []string{"outcome"})

func init() { metrics.Register(unansweredReleases) }

// ReleaseUnansweredOn runs the releases for reserves whose reply was lost on post's store, so they do not queue behind the admission calls that just timed out. Call before serving.
func (l *Limiter) ReleaseUnansweredOn(post *Limiter) *Limiter {
	if post != nil && post.runner != nil {
		l.releaser = post.runner
	}
	return l
}

// releaseUnanswered frees, in the background, the concurrency leases a failed reserve may have taken. The release is the commit script with no observations: it removes only this token's lease and writes its commit guard, which the reserve script checks, so it is a no-op when the reserve never ran and keeps a reserve that runs after it from taking the lease.
//
// Bucket charges stay: whether the reserve ran is unknown, and refunding a charge it never made would credit the bucket beyond its limit.
func (l *Limiter) releaseUnanswered(scope string, rules []Rule, token string, err error) {
	if l.releaser == nil || !mayHaveRun(err) || !holdsLease(rules) {
		return
	}
	select {
	case l.releaseSlots <- struct{}{}:
	default:
		unansweredReleases.WithLabelValues("skipped").Inc()
		return
	}
	res := &Reservation{ID: token, scope: scope}
	for _, r := range rules {
		if r.Meter == "concurrency" {
			res.conKeys = append(res.conKeys, concurrencyKey(scope, r))
		}
	}
	call, err := l.buildCommitCall(res, Observations{}, l.clock())
	if err != nil {
		<-l.releaseSlots
		unansweredReleases.WithLabelValues("failed").Inc()
		return
	}
	l.releases.Add(1)
	go func() {
		defer func() {
			<-l.releaseSlots
			l.releases.Done()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), unansweredReleaseTimeout)
		defer cancel()
		if _, err := l.releaser.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...); err != nil {
			unansweredReleases.WithLabelValues("failed").Inc()
			return
		}
		unansweredReleases.WithLabelValues("released").Inc()
	}()
}

// mayHaveRun reports a reserve failure after which the script may still have run: the store took the call and never answered, or the caller left while it was in flight.
func mayHaveRun(err error) bool {
	if errors.Is(err, kv.ErrNotRun) {
		return false
	}
	return errors.Is(err, kv.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
