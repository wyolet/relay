package ratelimit

import (
	"fmt"

	"github.com/wyolet/relay/app/policy"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Resolve produces the []pkgratelimit.Rule the limiter understands from
// a Policy + its attached RateLimit. The pipeline calls pkg/ratelimit
// directly with this slice; there is no intermediate adapter wrapper.
//
// Key construction:
//
//   "policy:{policy-slug}:{rule-index}:{meter}"
//
// — policy-slug makes the scope visible in dashboards; rule-index keeps
// multi-rule RateLimits independent so e.g. "100 req/min AND 1M tok/hour"
// don't clobber each other.
//
// Name construction:
//
//   "{meter} on {policy-slug}"
//
// — surfaced to callers in 429 error messages.
//
// Returns nil when rl is nil or has no Rules.
func Resolve(pol *policy.Policy, rl *RateLimit) []pkgratelimit.Rule {
	if rl == nil || len(rl.Spec.Rules) == 0 || (rl.Spec.Enabled != nil && !*rl.Spec.Enabled) {
		return nil
	}
	if pol == nil {
		return nil
	}
	scope := pol.Meta.Name
	out := make([]pkgratelimit.Rule, 0, len(rl.Spec.Rules))
	for i, r := range rl.Spec.Rules {
		out = append(out, pkgratelimit.Rule{
			Key:      fmt.Sprintf("policy:%s:%d:%s", scope, i, r.Meter),
			Name:     fmt.Sprintf("%s on %s", r.Meter, scope),
			Meter:    string(r.Meter),
			Strategy: pkgratelimit.Strategy(r.Strategy),
			Amount:   r.Amount,
			Window:   r.Window,
		})
	}
	return out
}
