package plan

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/sdk/usage"
)

func wantHops(t *testing.T, r *Route, want ...[]string) {
	t.Helper()
	if r.Refused != nil {
		t.Fatalf("route refused: %v", r.Refused)
	}
	got := slotIDs(r)
	if len(got) != len(want) {
		t.Fatalf("hops = %v, want %v", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("hop %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func wantRefused(t *testing.T, r *Route, code string, rung limits.Rung) *Refusal {
	t.Helper()
	if r.Refused == nil {
		t.Fatalf("route served %v, want refusal %s", slotIDs(r), code)
	}
	if r.Refused.Code != code || r.Refused.Rung != rung {
		t.Fatalf("refusal = %s on %q, want %s on %q (%v)", r.Refused.Code, r.Refused.Rung, code, rung, r.Refused)
	}
	return r.Refused
}

func TestRoute_NoCallerPolicyReachesOwnScopesInnermostFirst(t *testing.T) {
	b := newBasic()
	own := b.w.cred(b.hostB, b.acctB, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	other := b.w.project(b.team)
	b.w.cred(b.hostA, b.acctA, meta.Owner{Kind: meta.OwnerProject, ID: other.Meta.ID})
	u := b.w.user()
	uk := b.w.userKey(u)
	mine := b.w.cred(b.hostA, b.acctA, meta.Owner{Kind: meta.OwnerUser, ID: u.ID})
	h := b.w.start(t)

	wantHops(t, h.route(b.key, "m1"), ids(own), ids(b.a1), ids(b.a2), ids(b.b1))
	wantHops(t, h.route(uk, "m1"), ids(mine), ids(b.a1), ids(b.a2), ids(b.b1))
}

func TestRoute_InnermostLinkingRungOrdersPoolsAndOuterRungsNarrow(t *testing.T) {
	b := newBasic()
	poolA := b.w.pool(b.hostA, b.a1, b.a2)
	poolB := b.w.pool(b.hostB, b.b1)
	stray := b.w.cred(b.hostA, b.acctA, system)
	strayPool := b.w.pool(b.hostA, stray, b.a2)
	teamPol := b.w.policy("team", policy.Spec{PoolIDs: []string{poolA.Meta.ID, poolB.Meta.ID}})
	saPol := b.w.policy("sa", policy.Spec{PoolIDs: []string{poolB.Meta.ID, strayPool.Meta.ID}})
	b.w.bindPolicy(teamPol, b.w.teamAccount(b.team))
	b.w.bindPolicy(saPol, b.w.saAccount(b.sa))
	h := b.w.start(t)

	// The lane's order wins; the stray credential is outside the team's pools and is narrowed away.
	wantHops(t, h.route(b.key, "m1"), ids(b.b1), ids(b.a2))
	if hop := h.route(b.key, "m1").Hops[0]; hop.Pool.Pool == nil || hop.Pool.Pool.Meta.ID != poolB.Meta.ID || hop.Binding.Spec.HostID != b.hostB.Meta.ID {
		t.Fatalf("first hop = pool %v at %s, want pool B", hop.Pool.Pool, hop.Binding.Spec.HostID)
	}
}

func TestRoute_PinnedHostKeepsOnlyItsHops(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1@host-b"), ids(b.b1))
}

func TestRoute_CallerGrantRefusalNamesRungAndPolicy(t *testing.T) {
	b := newBasic()
	b.w.model(b.prov, "other")
	p := b.w.policy("only-other", policy.Spec{Models: []string{"acme/other"}})
	projAcct := b.w.projectAccount(b.proj)
	b.w.bindPolicy(p, projAcct)
	h := b.w.start(t)

	r := wantRefused(t, h.route(b.key, "m1"), CodeModelNotAllowed, limits.RungProject)
	if r.PolicyID != p.Meta.ID || r.SubjectID != "account:"+projAcct.Meta.ID {
		t.Fatalf("refusal names %s/%s, want the project account's policy", r.SubjectID, r.PolicyID)
	}
}

// A provider wildcard ("acme/*") grants every model of that provider and no other, in the grant and in a rate limit's models scope, even stored as written.
func TestRoute_ProviderWildcardGrantsTheProvidersModels(t *testing.T) {
	b := newBasic()
	beta := b.w.provider("beta")
	x := b.w.model(beta, "x")
	b.w.bind(x, b.hostA)
	b.w.price(x, b.hostA)
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	tier := b.w.policy("acme-tier", policy.Spec{Models: []string{"acme/*", "beta/*"}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID, Models: []string{"acme/*"}}}})
	b.w.bindPolicy(tier, b.acctA)
	p := b.w.policy("acme-only", policy.Spec{Models: []string{"acme/*"}})
	b.w.bindPolicy(p, b.w.projectAccount(b.proj))
	h := b.w.start(t)

	wantHops(t, h.route(b.key, "acme/m1@host-a"), ids(b.a1), ids(b.a2))
	_ = wantRefused(t, h.route(b.key, "beta/x"), CodeModelNotAllowed, limits.RungProject)
	byModel := get[KeySlot](h, b.a1.Meta.ID).ByModel
	if len(byModel[b.m1.Meta.ID]) != 1 || len(byModel[x.Meta.ID]) != 0 {
		t.Fatalf("scoped account counters by model = %v, want one for m1 and none for x", byModel)
	}
}

// A disabled policy refuses on its rung, never falls through to a broader one, keeps its dependents and serves again once re-enabled.
func TestRoute_DisabledPolicyRefusesOnItsRungWithoutFallThrough(t *testing.T) {
	b := newBasic()
	off := false
	broad := b.w.policy("broad", policy.Spec{})
	p := b.w.policy("narrow", policy.Spec{Enabled: &off})
	b.w.bindPolicy(broad, b.w.teamAccount(b.team))
	b.w.bindPolicy(p, b.w.projectAccount(b.proj))
	h := b.w.start(t)

	kp := h.keyPlan(b.key)
	if kp == nil {
		t.Fatal("a key under a disabled policy lost its plan")
	}
	if kp.Caller.Refused == nil || kp.Caller.Refused.Code != CodePolicyDisabled || kp.Caller.Refused.Rung != limits.RungProject {
		t.Fatalf("caller refusal = %v, want policy_disabled on the project rung", kp.Caller.Refused)
	}
	_ = wantRefused(t, h.route(b.key, "m1"), CodePolicyDisabled, limits.RungProject)

	on := clone(p)
	on.Spec.Enabled = nil
	apply(h, on)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

func TestRoute_DuplicateBindingRefusesAndFlagsBoth(t *testing.T) {
	b := newBasic()
	p1, p2 := b.w.policy("p1", policy.Spec{}), b.w.policy("p2", policy.Spec{})
	pb1 := b.w.bindPolicy(p1, b.w.projectAccount(b.proj))
	pb2 := b.w.bindPolicy(p2, b.w.projectAccount(b.proj))
	h := b.w.start(t)

	_ = wantRefused(t, h.route(b.key, "m1"), CodeDuplicateBinding, limits.RungProject)
	for _, pb := range []string{pb1.Meta.ID, pb2.Meta.ID} {
		h.wantIssue(KindPolicyBinding, pb, CodeDuplicateBinding, true)
	}
	del(h, KindPolicyBinding, pb2.Meta.ID)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
	h.wantIssue(KindPolicyBinding, pb1.Meta.ID, CodeDuplicateBinding, false)
}

// A binding whose owner's scope does not hold the account (a team binding a user's personal account) is ignored for it and says so.
func TestRoute_BindingOutsideItsOwnersScopeIsIgnored(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	uk := b.w.userKey(u)
	p := b.w.policy("p", policy.Spec{Models: []string{"acme/none"}})
	pb := b.w.bindPolicy(p, b.w.personal(u))
	pb.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	h := b.w.start(t)
	h.wantIssue(KindPolicyBinding, pb.Meta.ID, IssueSubjectOutOfScope, true)
	wantHops(t, h.route(uk, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

// A missing pool narrows the route and raises ref_missing on the policy; restoring it restores the route.
func TestRoute_MissingPoolNarrowsAndReturns(t *testing.T) {
	b := newBasic()
	poolA := b.w.pool(b.hostA, b.a1)
	poolB := b.w.pool(b.hostB, b.b1)
	p := b.w.policy("p", policy.Spec{PoolIDs: []string{poolA.Meta.ID, poolB.Meta.ID}})
	b.w.bindPolicy(p, b.w.projectAccount(b.proj))
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.b1))

	del(h, KindPool, poolA.Meta.ID)
	wantHops(t, h.route(b.key, "m1"), ids(b.b1))
	h.wantIssue(KindPolicy, p.Meta.ID, IssueRefMissing, true)
	apply(h, poolA)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.b1))
	h.wantIssue(KindPolicy, p.Meta.ID, IssueRefMissing, false)
}

// A disabled tier policy takes its account's credentials out instead of letting them serve unmetered.
func TestRoute_DisabledAccountPolicyTakesItsCredentialsOut(t *testing.T) {
	b := newBasic()
	off := false
	tier := b.w.policy("tier", policy.Spec{Enabled: &off})
	b.w.bindPolicy(tier, b.acctA)
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.b1))
	_ = wantRefused(t, h.route(b.key, "m1@host-a"), CodePolicyDisabled, limits.RungAccount)
}

// A disabled host is not routed to: the route fails over to the other host, and with both disabled the model has no binding to serve it.
func TestRoute_SkipsDisabledHost(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	off := clone(b.hostA)
	off.Spec.Enabled = boolp(false)
	apply(h, off)
	r := h.route(b.key, "m1")
	if r.Refused != nil {
		t.Fatalf("refused with host-b enabled: %v", r.Refused)
	}
	for _, hop := range r.Hops {
		if hop.Host.Meta.ID == b.hostA.Meta.ID {
			t.Fatalf("route reaches disabled host-a: %v", slotIDs(r))
		}
	}

	offB := clone(b.hostB)
	offB.Spec.Enabled = boolp(false)
	apply(h, offB)
	if r := h.route(b.key, "m1"); r.Refused == nil || r.Refused.Code != CodeNoBinding {
		t.Fatalf("both hosts disabled: refused %+v, want %s", r.Refused, CodeNoBinding)
	}
}

// A policy whose pools reach the model's host but hold no usable key refuses with no_credentials, naming the rung that links them, not model_disabled.
func TestRoute_PoolWithNoUsableKeyRefusesNoCredentials(t *testing.T) {
	b := newBasic()
	b.a1.Spec.Enabled = boolp(false)
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
	tier := b.w.policy("tier-off", policy.Spec{Enabled: boolp(false)})
	b.w.bindPolicy(tier, b.acctA)
	h := b.w.start(t)

	if r := h.route(b.key, "m1").Refused; r == nil || r.Code != CodePolicyDisabled {
		t.Fatalf("refused %+v, want %s", r, CodePolicyDisabled)
	}
}

func TestRoute_UpstreamGrantNarrowsSlots(t *testing.T) {
	b := newBasic()
	b.w.model(b.prov, "other")
	own := b.w.account(b.hostA)
	b.a1.Spec.AccountID = own.Meta.ID
	tier := b.w.policy("tier", policy.Spec{Models: []string{"acme/other"}})
	b.w.bindPolicy(tier, own)
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.a2), ids(b.b1))
}

func TestRoute_UnpricedUnderBudgetIsRefusedAndFlagged(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	bud := b.w.budget("50")
	p := b.w.policy("capped", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID}}})
	pb := b.w.bindPolicy(p, b.w.teamAccount(b.team))
	h := b.w.start(t)

	r := wantRefused(t, h.route(b.key, "m2"), CodeUnpricedUnderBudget, limits.RungTeam)
	if r.PolicyID != p.Meta.ID {
		t.Fatalf("refusal names policy %s, want the budgeted one", r.PolicyID)
	}
	h.wantIssue(KindPolicy, p.Meta.ID, CodeUnpricedUnderBudget, true)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))

	// Without a budget the unpriced binding serves, marked unpriced.
	del(h, KindPolicyBinding, pb.Meta.ID)
	if hop := h.route(b.key, "m2").Hops[0]; hop.Priced || hop.Pricing != nil {
		t.Fatal("an unpriced hop is marked priced")
	}
}

// A disabled pricing costs nothing, so under a budget it is refused and flagged like no pricing at all, with or without the caller's own key.
func TestRoute_DisabledPricingUnderBudgetIsRefusedAndFlagged(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	pr := b.w.price(m2, b.hostA)
	pr.Spec.Enabled = boolp(false)
	bud := b.w.budget("50")
	p := b.w.policy("capped", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID}}})
	pb := b.w.bindPolicy(p, b.w.teamAccount(b.team))
	h := b.w.start(t)

	_ = wantRefused(t, h.route(b.key, "m2"), CodeUnpricedUnderBudget, limits.RungTeam)
	wantUnkeyedRefused(t, h.keyPlan(b.key).Caller.Routes.ResolveUnkeyed("m2"), CodeUnpricedUnderBudget, limits.RungTeam)
	h.wantIssue(KindPolicy, p.Meta.ID, CodeUnpricedUnderBudget, true)

	on := clone(pr)
	on.Spec.Enabled = boolp(true)
	apply(h, on)
	wantHops(t, h.route(b.key, "m2"), ids(b.a1), ids(b.a2))
	h.wantIssue(KindPolicy, p.Meta.ID, CodeUnpricedUnderBudget, false)
	if hop := h.route(b.key, "m2").Hops[0]; !hop.Priced || hop.Pricing == nil {
		t.Fatal("an enabled pricing's hop is marked unpriced")
	}

	// Without a budget the disabled pricing's binding serves unpriced, naming no pricing.
	apply(h, pr)
	del(h, KindPolicyBinding, pb.Meta.ID)
	if hop := h.route(b.key, "m2").Hops[0]; hop.Priced || hop.Pricing != nil {
		t.Fatalf("a disabled pricing's hop: priced %v, pricing %v; want unpriced and none", hop.Priced, hop.Pricing)
	}
	if u := h.keyPlan(b.key).Caller.Routes.ResolveUnkeyed("m2"); u.Refused != nil || u.Pricing != nil {
		t.Fatalf("a disabled pricing's unkeyed route = %+v, want served naming no pricing", u)
	}
}

// A disabled sheet never stands in for an enabled one: not when it is the later row covering the same model and host, nor when the binding names it while an enabled sheet covers the pair. Either binding is priced and charged under a budget.
func TestRoute_DisabledPricingNeverShadowsAnEnabledOne(t *testing.T) {
	b := newBasic()
	shadowed, named := b.w.model(b.prov, "shadowed"), b.w.model(b.prov, "named")
	b.w.bind(shadowed, b.hostA)
	enabledShadowed := b.w.price(shadowed, b.hostA)
	b.w.price(shadowed, b.hostA).Spec.Enabled = boolp(false)
	draft := b.w.price(named, b.hostA)
	draft.Spec.Enabled = boolp(false)
	enabledNamed := b.w.price(named, b.hostA)
	b.w.bind(named, b.hostA).Spec.PricingID = draft.Meta.ID
	bud := b.w.budget("50")
	p := b.w.policy("capped", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID}}})
	b.w.bindPolicy(p, b.w.teamAccount(b.team))
	h := b.w.start(t)

	for ref, want := range map[string]*pricing.Pricing{"shadowed": enabledShadowed, "named": enabledNamed} {
		r := h.route(b.key, ref)
		if r.Refused != nil {
			t.Fatalf("%s refused: %v", ref, r.Refused)
		}
		hop := r.Hops[0]
		if !hop.Priced || hop.Pricing == nil || hop.Pricing.Meta.ID != want.Meta.ID {
			t.Fatalf("%s: priced %v with %v, want the enabled sheet %s", ref, hop.Priced, hop.Pricing, want.Meta.Name)
		}
		if cost, _, ok := hop.Pricing.CostNanos(usage.Tokens{"input": 1_000_000}); !ok || cost == 0 {
			t.Errorf("%s: cost %d (ok %v), want a charge", ref, cost, ok)
		}
		if u := h.keyPlan(b.key).Caller.Routes.ResolveUnkeyed(ref); u.Refused != nil || u.Pricing == nil || u.Pricing.Meta.ID != want.Meta.ID {
			t.Fatalf("%s unkeyed = %+v, want served with the enabled sheet", ref, u)
		}
	}
	h.wantIssue(KindPolicy, p.Meta.ID, CodeUnpricedUnderBudget, false)
}

func TestRoute_UpstreamBudgetRefusesUnpricedOnThatSlotOnly(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	b.w.bind(m2, b.hostB)
	b.w.price(m2, b.hostB)
	bud := b.w.budget("50")
	tier := b.w.policy("tier", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID}}})
	b.w.bindPolicy(tier, b.acctA)
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m2"), ids(b.b1))
	_ = wantRefused(t, h.route(b.key, "m2@host-a"), CodeUnpricedUnderBudget, limits.RungAccount)
}

func wantUnkeyedRefused(t *testing.T, u *Unkeyed, code string, rung limits.Rung) {
	t.Helper()
	if u.Refused == nil || u.Refused.Code != code || u.Refused.Rung != rung {
		t.Fatalf("unkeyed = %+v, want refusal %s on %q", u, code, rung)
	}
}

// A request bringing its own key fits the caller rungs alone: no provider account's policy applies, as no relay credential serves, and a host is no rung.
func TestResolveUnkeyed_CallerRungsAloneApply(t *testing.T) {
	b := newBasic()
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	b.w.bindPolicy(b.w.policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), b.acctB)
	b.w.bindPolicy(b.w.policy("lane", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), b.w.saAccount(b.sa))
	h := b.w.start(t)
	u := h.keyPlan(b.key).Caller.Routes.ResolveUnkeyed("m1@host-b")
	if u.Refused != nil {
		t.Fatalf("m1@host-b refused: %v", u.Refused)
	}
	if n := len(u.Counters); n != 1 || u.Counters[0].Rung != limits.RungLane {
		t.Fatalf("counters = %+v, want the lane's one rule", u.Counters)
	}
}

// An unpriced binding is refused under a budget whether the request brings its own key or not, so a budget has no hole.
func TestResolveUnkeyed_UnpricedUnderBudgetIsRefused(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	bud := b.w.budget("50")
	p := b.w.policy("capped", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID}}})
	b.w.bindPolicy(p, b.w.teamAccount(b.team))
	h := b.w.start(t)
	table := h.keyPlan(b.key).Caller.Routes

	wantUnkeyedRefused(t, table.ResolveUnkeyed("m2"), CodeUnpricedUnderBudget, limits.RungTeam)
	u := table.ResolveUnkeyed("m1")
	if u.Refused != nil || u.Pricing == nil || len(u.Counters) != 1 || u.Counters[0].Kind != limits.KindBudget {
		t.Fatalf("priced m1 = %+v, want served under the team budget", u)
	}
}

// A request naming no caller resolves its model at the host it was sent to, only to price and label it: a model the catalog does not serve there leaves the model unset and still names the host.
func TestResolveAtHost(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Aliases = []string{"m1-latest"}
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostB)
	gone := b.w.model(b.prov, "gone")
	gone.Spec.Enabled = boolp(false)
	b.w.bind(gone, b.hostA)
	root := b.w.start(t).holder.Load()
	for _, tc := range []struct {
		name, ref       string
		model           string
		priced          bool
		upstream, alias string
	}{
		{name: "priced model", ref: "m1", model: "m1", priced: true, upstream: "m1"},
		{name: "provider-qualified", ref: "acme/m1", model: "m1", priced: true, upstream: "m1"},
		{name: "alias keeps its wire name", ref: "m1-latest", model: "m1", priced: true, upstream: "m1-latest", alias: "alias:m1-latest"},
		{name: "pinned to this host", ref: "m1@host-a", model: "m1", priced: true, upstream: "m1"},
		{name: "a pin to another host does not move the request", ref: "m1@host-b", model: "m1", priced: true, upstream: "m1"},
		{name: "served at another host only", ref: "m2"},
		{name: "disabled model", ref: "gone"},
		{name: "unknown model", ref: "nope"},
		{name: "no model named"},
	} {
		u := root.ResolveAtHost(b.hostA, tc.ref)
		if u.Host != b.hostA || u.Refused != nil || len(u.Counters) != 0 {
			t.Errorf("%s: host %v refused %v counters %v, want host-a and nothing refused or counted", tc.name, u.Host, u.Refused, u.Counters)
		}
		if tc.model == "" {
			if u.Model != nil || u.Binding != nil || u.Pricing != nil {
				t.Errorf("%s: resolved %+v, want no model", tc.name, u)
			}
			continue
		}
		if u.Model == nil || u.Model.Meta.Name != tc.model || u.Provider != "acme" || u.Binding.Spec.HostID != b.hostA.Meta.ID || (u.Pricing != nil) != tc.priced || u.UpstreamModel != tc.upstream || u.ResolvedVia != tc.alias {
			t.Errorf("%s: resolved %+v, want %s priced=%v sent as %q via %q", tc.name, u, tc.model, tc.priced, tc.upstream, tc.alias)
		}
	}
	if u := (&Root{}).ResolveAtHost(b.hostA, "m1"); u.Model != nil || u.Host != b.hostA {
		t.Errorf("without a catalog: %+v, want the host alone", u)
	}
}

func TestRoute_TableCachesFills(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	kp := h.keyPlan(b.key)
	r1, r2 := kp.Caller.Routes.Get("m1"), kp.Caller.Routes.Get("m1")
	if r1 != r2 || kp.Caller.Routes.Misses() != 1 {
		t.Fatalf("second Get refilled: same=%v misses=%d", r1 == r2, kp.Caller.Routes.Misses())
	}
	if kp.Caller.Routes.Get("nope").Refused.Code != CodeModelNotFound || kp.Caller.Routes.Misses() != 2 {
		t.Fatal("unknown names are not cached as refusals")
	}
}

func TestRoute_AliasCarriesItsWireName(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Aliases = []string{"m1-latest"}
	h := b.w.start(t)
	r := h.route(b.key, "m1-latest")
	if r.Refused != nil || r.UpstreamModel != "m1-latest" || r.ResolvedVia != "alias:m1-latest" {
		t.Fatalf("alias route = %+v", r)
	}
	if r := h.route(b.key, "m1"); r.UpstreamModel != "m1" || r.ResolvedVia != "" {
		t.Fatalf("plain route = upstream %q via %q", r.UpstreamModel, r.ResolvedVia)
	}
}

// A binding's upstreamName is the wire model name through it, over the snapshot's name and over an alias the host does not know; ResolvedVia still tags the alias. A hop through a binding without one keeps the alias's or the snapshot's name. m1 has one snapshot, so the name applies whether the binding lists it or lists none.
func TestRoute_BindingUpstreamNameIsTheWireName(t *testing.T) {
	const wire = "global.acme/m1-v1:0"
	for _, shape := range []struct {
		name         string
		upstreamName string
		snapshots    []string
	}{
		{name: "no upstream name"},
		{name: "lists the one snapshot", upstreamName: wire, snapshots: []string{"m1"}},
		{name: "lists none", upstreamName: wire},
	} {
		named := shape.upstreamName != ""
		b := newBasic()
		b.m1.Spec.Aliases = []string{"m1-latest", "m1[*]"}
		bd := b.w.bindingAt(b.m1, b.hostA)
		bd.Spec.UpstreamName, bd.Spec.Snapshots = shape.upstreamName, shape.snapshots
		h := b.w.start(t)
		for _, tc := range []struct {
			ref, fallback, via string
			firstAtA           bool
		}{
			{ref: "m1", fallback: "m1", firstAtA: true},
			{ref: "acme/m1", fallback: "m1", firstAtA: true},
			{ref: "m1@host-a", fallback: "m1", firstAtA: true},
			{ref: "m1@host-b", fallback: "m1"},
			{ref: "m1-latest", fallback: "m1-latest", via: "alias:m1-latest", firstAtA: true},
			{ref: "m1-latest@host-a", fallback: "m1-latest", via: "alias:m1-latest", firstAtA: true},
			{ref: "m1[1m]", fallback: "m1[1m]", via: "alias:m1[*]", firstAtA: true},
		} {
			sentAtA := tc.fallback
			if named {
				sentAtA = wire
			}
			want := tc.fallback
			if tc.firstAtA {
				want = sentAtA
			}
			r := h.route(b.key, tc.ref)
			if r.Refused != nil || r.UpstreamModel != want || r.ResolvedVia != tc.via {
				t.Errorf("%s, %s: route sends %q via %q (refused %v), want %q via %q", shape.name, tc.ref, r.UpstreamModel, r.ResolvedVia, r.Refused, want, tc.via)
			}
			for i := range r.Hops {
				hop := &r.Hops[i]
				hopWant := tc.fallback
				if hop.Host.Meta.ID == b.hostA.Meta.ID {
					hopWant = sentAtA
				}
				if got := hop.UpstreamModel; got != hopWant {
					t.Errorf("%s, %s: hop %d at %s sends %q, want %q", shape.name, tc.ref, i, hop.Host.Meta.Name, got, hopWant)
				}
			}
		}
	}
}

// One upstreamName names one upstream model. On a binding serving several snapshots (a row listing none, of a model with several) it is ignored and every snapshot is sent its own name, as without it; a binding listing one snapshot sends the name for that snapshot and serves no other.
func TestRoute_UpstreamNameAppliesOnlyToOneServedSnapshot(t *testing.T) {
	const wire = "global.acme/m1-v1:0"
	for _, tc := range []struct {
		name      string
		snapshots []string
		want      map[string][]string // ref → the name each hop is sent, host-a's first
	}{
		{"lists none", nil, map[string][]string{"m1": {"m1", "m1", "m1"}, "m1-2026-01-01": {"m1-2026-01-01", "m1-2026-01-01", "m1-2026-01-01"}}},
		{"lists one", []string{"m1"}, map[string][]string{"m1": {wire, wire, "m1"}, "m1-2026-01-01": {"m1-2026-01-01"}}},
	} {
		b := newBasic()
		b.m1.Spec.Snapshots = append(b.m1.Spec.Snapshots, model.Snapshot{Name: "m1-2026-01-01"})
		bd := b.w.bindingAt(b.m1, b.hostA)
		bd.Spec.UpstreamName, bd.Spec.Snapshots = wire, tc.snapshots
		h := b.w.start(t)
		for ref, want := range tc.want {
			r := h.route(b.key, ref)
			var got []string
			for _, hop := range r.Hops {
				got = append(got, hop.UpstreamModel)
			}
			if r.Refused != nil || !slices.Equal(got, want) || r.UpstreamModel != want[0] {
				t.Errorf("%s, %s: hops send %v, route %q (refused %v); want %v", tc.name, ref, got, r.UpstreamModel, r.Refused, want)
			}
		}
	}
}

// An alias resolves to its model but the ladder still gates it.
func TestRoute_AliasStillPassesTheGate(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Aliases = []string{"m1-latest"}
	b.w.model(b.prov, "other")
	p := b.w.policy("p", policy.Spec{Models: []string{"acme/other"}})
	b.w.bindPolicy(p, b.w.projectAccount(b.proj))
	h := b.w.start(t)
	_ = wantRefused(t, h.route(b.key, "m1-latest"), CodeModelNotAllowed, limits.RungProject)
}

// A rotated key authenticates on both hashes until its grace ends, whichever shard the previous hash lands in; expiry and revocation end the key.
func TestKeyPlan_GraceAndExpiry(t *testing.T) {
	old := "aa" + keyHash("old")[2:]
	for _, tc := range []struct{ name, rotated string }{
		{"previous hash in the same shard", "aa" + keyHash("new")[2:]},
		{"previous hash in another shard", "bb" + keyHash("new")[2:]},
		{"upper-case hash", "BB" + keyHash("new")[2:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBasic()
			b.key.Spec.KeyHash = old
			h := b.w.start(t)
			now := time.Now()
			until := now.Add(time.Hour)
			rot := clone(b.key)
			rot.Spec.PreviousKeyHash, rot.Spec.KeyHash, rot.Spec.GraceUntil = old, tc.rotated, &until
			apply(h, rot)
			idx := h.holder.Load().Keys
			kp := idx.Lookup(tc.rotated, now)
			if kp == nil || kp.Key.Meta.ID != b.key.Meta.ID || !kp.GraceEnds.Equal(until) {
				t.Fatalf("rotated hash = %+v, want the key's plan with grace ending %v", kp, until)
			}
			if got := idx.Lookup(old, until.Add(-time.Nanosecond)); got != kp {
				t.Fatalf("previous hash inside grace = %p, want the rotated key's plan %p", got, kp)
			}
			if idx.Lookup(old, until) != nil || idx.Lookup(tc.rotated, until) != kp {
				t.Fatal("at the end of grace the previous hash must stop and the rotated one go on")
			}

			exp := clone(rot)
			exp.Spec.ExpiresAt = &until
			apply(h, exp)
			idx = h.holder.Load().Keys
			if idx.Lookup(tc.rotated, until.Add(-time.Nanosecond)) == nil || idx.Lookup(tc.rotated, until) != nil {
				t.Fatal("a key must authenticate until its expiry and not at it")
			}

			noGrace := clone(rot)
			noGrace.Spec.GraceUntil = nil
			apply(h, noGrace)
			if h.holder.Load().Keys.Lookup(old, now) != nil {
				t.Fatal("a previous hash with no grace window authenticated")
			}

			revoked := clone(rot)
			revoked.Spec.RevokedAt = &now
			apply(h, revoked)
			if get[KeyPlan](h, revoked.Meta.ID) != nil || h.holder.Load().Keys.Lookup(tc.rotated, now) != nil {
				t.Fatal("a revoked key kept its plan")
			}
		})
	}
}

// A hash another key holds in its rotation grace never takes the slot of the key whose current hash it is.
func TestKeyPlan_CurrentHashBeatsAnotherKeysGraceHash(t *testing.T) {
	b := newBasic()
	until := time.Now().Add(time.Hour)
	for range 8 {
		k := b.w.saKey(b.sa)
		k.Spec.PreviousKeyHash, k.Spec.GraceUntil = b.key.Spec.KeyHash, &until
	}
	h := b.w.start(t)
	if kp := h.keyPlan(b.key); kp == nil || kp.Key.Meta.ID != b.key.Meta.ID {
		t.Fatalf("the live hash resolves to %+v, want its own key", kp)
	}
}

func TestKeyPlanIndex_LookupMisses(t *testing.T) {
	b := newBasic()
	b.key.Spec.KeyHash = "aa" + keyHash("k")[2:]
	idx := b.w.start(t).holder.Load().Keys
	for name, hash := range map[string]string{
		"empty":                     "",
		"one character":             "a",
		"not hex":                   "zz" + keyHash("k")[2:],
		"half hex":                  "az" + keyHash("k")[2:],
		"another hash in the shard": "aa" + keyHash("other")[2:],
		"a shard with no key":       "cc" + keyHash("k")[2:],
	} {
		if kp := idx.Lookup(hash, time.Now()); kp != nil {
			t.Errorf("%s: Lookup(%q) = %+v, want nil", name, hash, kp)
		}
	}
	if (*KeyPlanIndex)(nil).Lookup(b.key.Spec.KeyHash, time.Now()) != nil {
		t.Error("a nil index found a key")
	}
}

func TestCells_SharedPerAccountKeyedByValueAndPool(t *testing.T) {
	b := newBasic()
	poolA := b.w.pool(b.hostA, b.a1, b.a2)
	p1 := b.w.policy("p1", policy.Spec{PoolIDs: []string{poolA.Meta.ID}})
	p2 := b.w.policy("p2", policy.Spec{PoolIDs: []string{poolA.Meta.ID}})
	b.w.bindPolicy(p1, b.w.projectAccount(b.proj))
	other := b.w.sa(b.w.project(b.team))
	b.w.bindPolicy(p2, b.w.saAccount(other))
	otherKey := b.w.saKey(other)
	h := b.w.start(t)

	s1, s2 := get[KeySlot](h, b.a1.Meta.ID), get[KeySlot](h, b.a2.Meta.ID)
	if s1.Account.Cooldown != s2.Account.Cooldown {
		t.Fatal("credentials of one account do not share its cooldown")
	}
	if s1.Breaker == s2.Breaker || s1.Breaker == nil {
		t.Fatal("two credential values share a breaker")
	}
	r1, r2 := h.route(b.key, "m1").Hops[0].Pool, h.route(otherKey, "m1").Hops[0].Pool
	if r1.Rotation == nil || r1.Rotation != r2.Rotation {
		t.Fatal("two policies on one pool do not share its rotation")
	}
}

func TestIssues_ParentMissingParksTheRow(t *testing.T) {
	b := newBasic()
	b.a1.Spec.AccountID = meta.NewID()
	h := b.w.start(t)
	if get[KeySlot](h, b.a1.Meta.ID) != nil {
		t.Fatal("a credential whose account is missing serves")
	}
	h.wantIssue(KindCredential, b.a1.Meta.ID, IssueRefMissing, true)
	wantHops(t, h.route(b.key, "m1"), ids(b.a2), ids(b.b1))
}
