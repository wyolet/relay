// Package limits declares what admission counts and the interface that counts it. A Counter describes one rate-limit rule or one budget a request must fit; plans build the list per route when they are reconciled (app/plan), and a Limiter admits a request against that list and settles it after the response.
//
// Scope: the descriptor, its identity and atomicity group, and the Limiter and Reservation interfaces. Out of scope: every limiter implementation, store key formats, window and budget arithmetic, and deciding which counters a request touches.
package limits

import (
	"context"
	"strconv"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

// Kind tells a rule counter from a budget counter.
type Kind uint8

const (
	KindRule Kind = iota + 1
	KindBudget
)

// Rung is the level of the policy ladder a counter's policy is bound at.
type Rung string

const (
	RungHost       Rung = "host"
	RungAccount    Rung = "account"
	RungCredential Rung = "credential"
	RungTeam       Rung = "team"
	RungProject    Rung = "project"
	RungPrincipal  Rung = "principal"
	// RungSystem marks a relay-wide ceiling no policy binds, such as the proxy-mode limit.
	RungSystem Rung = "system"
)

// Upstream reports whether the rung is on the provider side of the ladder, where counters are shared by every caller that reaches the rung object.
func (r Rung) Upstream() bool {
	return r == RungHost || r == RungAccount || r == RungCredential
}

// Counter is one rate-limit rule or one budget a request must fit. Plans build it once; the request path hands it to a Limiter unchanged, so ID and Group are rendered here rather than per request.
type Counter struct {
	Kind     Kind
	Rung     Rung
	PolicyID string
	// SubjectID is the subject the policy is bound to, as "<kind>:<id>": a team, project or principal on the caller side; the host, account or credential upstream.
	SubjectID string
	// ModelID is set when an upstream rule is scoped to models; such a rule counts once per matched model.
	ModelID     string
	RateLimitID string
	Rule        ratelimit.Rule // KindRule
	Budget      *budget.Budget // KindBudget
	// ID is the counter's stable identity: equal IDs count in one bucket. It survives edits that do not touch the counter (adding a model to the policy, renaming anything).
	ID string
	// Group is the atomicity group: counters sharing it may be updated in one store call.
	Group string
}

// CallerRule describes a rule of a caller-side policy. Its identity is (rule, policy, bound subject): a policy reused by two subjects gives each its own bucket, and rebinding a subject to another policy starts a fresh one.
func CallerRule(rung Rung, rl *ratelimit.RateLimit, r ratelimit.Rule, policyID, subjectID, group string) Counter {
	return Counter{
		Kind: KindRule, Rung: rung, PolicyID: policyID, SubjectID: subjectID, RateLimitID: rl.Meta.ID, Rule: r,
		ID:    ruleContent(rl.Meta.ID, r) + ":p:" + policyID + ":s:" + subjectID,
		Group: group,
	}
}

// UpstreamRule describes a rule of a host, account or credential policy. Its identity is (rule, rung object, model) without the policy, so the provider's ceiling keeps its count when the rung is rebound to another tier policy that links the same rate limit, and every caller reaching the object shares it.
func UpstreamRule(rung Rung, rl *ratelimit.RateLimit, r ratelimit.Rule, policyID, subjectID, modelID, group string) Counter {
	id := ruleContent(rl.Meta.ID, r) + ":o:" + subjectID
	if modelID != "" {
		id += ":m:" + modelID
	}
	return Counter{
		Kind: KindRule, Rung: rung, PolicyID: policyID, SubjectID: subjectID, ModelID: modelID, RateLimitID: rl.Meta.ID, Rule: r,
		ID: id, Group: group,
	}
}

// SystemRule describes a rule of a system rate limit counted per subject (a key hash, a client IP). It counts in a group of its own, apart from any caller's.
func SystemRule(rl *ratelimit.RateLimit, r ratelimit.Rule, subject string) Counter {
	return Counter{
		Kind: KindRule, Rung: RungSystem, SubjectID: subject, RateLimitID: rl.Meta.ID, Rule: r,
		ID:    ruleContent(rl.Meta.ID, r) + ":s:" + subject,
		Group: "system:" + subject,
	}
}

// BudgetCounter describes a budget linked by a policy at any rung. Its identity is (budget, bound subject, period): an amount or mode edit keeps the spent count, a period change starts a new one.
func BudgetCounter(rung Rung, b *budget.Budget, policyID, subjectID, group string) Counter {
	return Counter{
		Kind: KindBudget, Rung: rung, PolicyID: policyID, SubjectID: subjectID, Budget: b,
		ID:    "b:" + b.Meta.ID + ":" + string(b.Spec.Period) + ":s:" + subjectID,
		Group: group,
	}
}

// ruleContent identifies a rule inside its rate limit the way the rate limit's own validation does (meter, strategy, window), so an amount edit keeps the count and a window edit starts a fresh one.
func ruleContent(rateLimitID string, r ratelimit.Rule) string {
	return "r:" + rateLimitID + ":" + string(r.Strategy) + ":" + strconv.FormatInt(int64(r.Window.Duration()/time.Second), 10) + ":" + string(r.Meter)
}

// Usage is what a request spends: tokens by kind and the priced cost. An estimate at admission, the real figures at settlement.
type Usage struct {
	Tokens    usage.Tokens
	CostNanos int64 // USD nanos
}

// Limiter admits a request against its counters and settles it after the response. Implementations are swappable; the plans never count.
type Limiter interface {
	// Admit checks every counter (rules reserve the estimate; budgets compare spent with the cap) and either reserves all of them or refuses with a *Refused naming the first counter that refused; on refusal nothing stays reserved. A store failure refuses: limits fail closed.
	Admit(ctx context.Context, cs []Counter, est Usage) (Reservation, error)
}

// Reservation is one admitted request's hold on its counters.
type Reservation interface {
	// Settle charges the real usage; called once, after the response, off the request path.
	Settle(actual Usage)
	// Release undoes the reservation when the request never reached the upstream.
	Release()
}

// Refused is the error Admit returns when a counter has no room.
type Refused struct {
	Counter    *Counter
	RetryAfter time.Duration
}

func (r *Refused) Error() string {
	return "limits: " + string(r.Counter.Rung) + " policy " + r.Counter.PolicyID + " refused counter " + r.Counter.ID
}
