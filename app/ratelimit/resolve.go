package ratelimit

import (
	"fmt"

	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// PerModelScope adds the request model id to a bucket key so per-model
// rules partition correctly. Used when a Policy.RLBinding has non-empty
// Models (otherwise the binding is "any model" and gets one shared
// bucket). The modelID suffix lives in the key, not the namespace, so
// the Lua hash-tag boundary still groups all of a key's buckets.
func PerModelScope(base, modelID string) string {
	if modelID == "" {
		return base
	}
	return base + ":m:" + modelID
}

// Resolve was here; moved to *policy.Policy.ResolveRules so the policy
// package can own its runtime methods without ratelimit importing
// policy (which would form a cycle once policy.Service lands). Use
// pol.ResolveRules(rl) instead.

// ResolveWithScope is the policy-less variant used by proxy mode, where
// the rate-limit subject is not a Policy but a per-key hash or per-IP
// identifier. namespace identifies the bucket family ("proxy",
// "proxy-anon"); subject is the request's bucket key (relay-key hash,
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
	for i, r := range rl.Spec.Rules {
		out = append(out, pkgratelimit.Rule{
			Key:      fmt.Sprintf("%s:%s:%d:%s", namespace, subject, i, r.Meter),
			Name:     fmt.Sprintf("%s on %s", r.Meter, subject),
			Meter:    string(r.Meter),
			Strategy: pkgratelimit.Strategy(r.Strategy),
			Amount:   r.Amount,
			Window:   r.Window,
		})
	}
	return out
}
