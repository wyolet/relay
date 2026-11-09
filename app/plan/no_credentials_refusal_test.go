package plan

import (
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/policy"
)

// A policy whose pools reach the model's host but hold no usable key refuses with no_credentials, naming the rung that links them, not model_disabled.
func TestRoute_PoolWithNoUsableKeyRefusesNoCredentials(t *testing.T) {
	b := newBasic()
	disabled := false
	b.a1.Spec.Enabled = &disabled
	p := b.w.pool(b.hostA, b.a1)
	pol := b.w.policy("dead-pool", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	b.w.bindPolicy(pol, b.w.teamAccount(b.team))
	h := b.w.start(t)

	r := h.route(b.key, "m1").Refused
	if r == nil || r.Code != CodeNoCredentials || r.Rung != limits.RungTeam || r.PolicyID != pol.Meta.ID {
		t.Fatalf("refused %+v, want %s on the team rung's policy", r, CodeNoCredentials)
	}
	if KindOf(r.Code) != RefusalHostNotReachable {
		t.Errorf("kind = %s, want %s", KindOf(r.Code), RefusalHostNotReachable)
	}
}

// A refusal from a later step still wins: a pool whose key the account's disabled policy refuses answers policy_disabled.
func TestRoute_LaterRefusalBeatsNoCredentials(t *testing.T) {
	b := newBasic()
	p := b.w.pool(b.hostA, b.a1)
	pol := b.w.policy("pool-a", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	b.w.bindPolicy(pol, b.w.teamAccount(b.team))
	disabled := false
	tier := b.w.policy("tier-off", policy.Spec{Enabled: &disabled})
	b.w.bindPolicy(tier, b.acctA)
	h := b.w.start(t)

	if r := h.route(b.key, "m1").Refused; r == nil || r.Code != CodePolicyDisabled {
		t.Fatalf("refused %+v, want %s", r, CodePolicyDisabled)
	}
}
