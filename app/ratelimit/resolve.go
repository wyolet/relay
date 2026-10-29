package ratelimit

import (
	"strconv"
	"strings"
	"time"

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
//	"{namespace}:{subject}:{strategy}:{window seconds}:{meter}"
//
// A rule is known by its content minus the amount, so reordering the rules or editing an amount keeps every counter; Validate rejects two rules with the same identity.
//
// Returns nil when rl is nil, disabled, or has no Rules.
func ResolveWithScope(namespace, subject string, rl *RateLimit) []pkgratelimit.Rule {
	if rl == nil || len(rl.Spec.Rules) == 0 || (rl.Spec.Enabled != nil && !*rl.Spec.Enabled) {
		return nil
	}
	out := make([]pkgratelimit.Rule, 0, len(rl.Spec.Rules))
	// Concatenation, not fmt: this runs per rule per request.
	prefix := namespace + ":" + subject + ":"
	for _, r := range rl.Spec.Rules {
		out = append(out, pkgratelimit.Rule{
			Key:      ruleKey(prefix, r),
			Name:     string(r.Meter) + " on " + subject,
			Meter:    string(r.Meter),
			Strategy: pkgratelimit.Strategy(r.Strategy),
			Amount:   r.Amount,
			Window:   r.Window.Duration(),
		})
	}
	return out
}

// ruleKey renders prefix + "<strategy>:<window seconds>:<meter>" in one allocation.
func ruleKey(prefix string, r Rule) string {
	var num [20]byte
	win := strconv.AppendInt(num[:0], windowSeconds(r.Window), 10)
	var b strings.Builder
	b.Grow(len(prefix) + len(r.Strategy) + 1 + len(win) + 1 + len(r.Meter))
	b.WriteString(prefix)
	b.WriteString(string(r.Strategy))
	b.WriteByte(':')
	b.Write(win)
	b.WriteByte(':')
	b.WriteString(string(r.Meter))
	return b.String()
}

// windowSeconds is the window at the granularity it is stored in, so a rule keeps its key across a round trip through Postgres.
func windowSeconds(w Window) int64 { return int64(w.Duration() / time.Second) }
