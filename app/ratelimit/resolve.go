package ratelimit

import (
	"strconv"

	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Resolve was here; moved to *policy.Policy.ResolveRules so the policy
// package can own its runtime methods without ratelimit importing
// policy (which would form a cycle once policy.Service lands). Use
// pol.ResolveRules(rl) instead.

// ResolveWithScope is the policy-less variant used by proxy mode, where
// the rate-limit subject is not a Policy but a per-key hash or per-IP
// identifier. namespace identifies the bucket family ("proxy",
// "proxy-anon"); subject is the request's bucket key (key hash,
// client IP, etc.). Key construction:
//
//	"{namespace}:{subject}:{rule-index}:{meter}"
//
// Returns nil when rl is nil, disabled, or has no Rules.
func ResolveWithScope(namespace, subject string, rl *RateLimit) []pkgratelimit.Rule {
	if rl == nil || len(rl.Spec.Rules) == 0 || (rl.Spec.Enabled != nil && !*rl.Spec.Enabled) {
		return nil
	}
	out := make([]pkgratelimit.Rule, 0, len(rl.Spec.Rules))
	// Concatenation, not fmt: this runs per rule per request.
	prefix := namespace + ":" + subject + ":"
	for i, r := range rl.Spec.Rules {
		out = append(out, pkgratelimit.Rule{
			Key:      prefix + strconv.Itoa(i) + ":" + string(r.Meter),
			Name:     string(r.Meter) + " on " + subject,
			Meter:    string(r.Meter),
			Strategy: pkgratelimit.Strategy(r.Strategy),
			Amount:   r.Amount,
			Window:   r.Window.Duration(),
		})
	}
	return out
}
