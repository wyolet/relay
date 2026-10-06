package otlpreceiver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	apprl "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// RateLimitName is the system RateLimit that caps export requests per credential.
const RateLimitName = "otlp-export"

// rateLimitNamespace keeps the receiver's counters apart from other limits keyed by the same credential.
const rateLimitNamespace = "otlp"

// DefaultExportsPerMinute is the per-credential cap applied when the catalog holds no RateLimitName row.
const DefaultExportsPerMinute = 1200

// defaultRateLimit stands in for a missing RateLimitName row. System rate limits reach a database only when an operator seeds them, so most deployments have no row; a row, enabled or disabled, always wins over this.
var defaultRateLimit = &apprl.RateLimit{Spec: apprl.Spec{Rules: []apprl.Rule{{
	Meter:    apprl.MeterRequests,
	Amount:   DefaultExportsPerMinute,
	Window:   apprl.Window(time.Minute),
	Strategy: apprl.StrategySlidingWindow,
}}}}

// rateLimited reserves one request for the reporter's credential and reports how long to wait when it is over the limit. A limiter that fails lets the export through: the limit protects the receiver, and telemetry lost to a kv outage is the worse outcome.
func (h *Handler) rateLimited(ctx context.Context, snap *appcatalog.Snapshot, reporter *lifecycle.Context) (time.Duration, bool) {
	if h.opts.Limiter == nil || snap == nil {
		return 0, false
	}
	// A key is known by its hash; a token has none and is known by its id.
	subject := reporter.RelayKeyHash
	if subject == "" {
		subject = reporter.CredentialID
	}
	if subject == "" {
		return 0, false
	}
	rl, ok := snap.RateLimitByName(RateLimitName)
	if !ok {
		rl = defaultRateLimit
	}
	rules := requestRules(apprl.ResolveWithScope(rateLimitNamespace, subject, rl))
	if len(rules) == 0 {
		return 0, false
	}
	_, err := h.opts.Limiter.Reserve(ctx, subject, rules)
	var exceeded *pkgratelimit.KeyQuotaExhausted
	switch {
	case errors.As(err, &exceeded):
		return exceeded.RetryAfter, true
	case err != nil:
		slog.Default().Warn("otlp receiver: rate limit check failed; accepting the export", "err", err)
	}
	return 0, false
}

// requestRules keeps the rules that count requests. An export has no tokens to meter, and a concurrency slot would need a second limiter call to give back.
func requestRules(rules []pkgratelimit.Rule) []pkgratelimit.Rule {
	out := rules[:0]
	for _, r := range rules {
		if r.Meter == string(apprl.MeterRequests) {
			out = append(out, r)
		}
	}
	return out
}
