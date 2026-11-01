package plan

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
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
	teamPol := b.w.policy("team", policy.Spec{PoolIDs: []string{poolA.Meta.ID, poolB.Meta.ID}})
	saPol := b.w.policy("sa", policy.Spec{PoolIDs: []string{poolB.Meta.ID}, CredentialIDs: []string{stray.Meta.ID, b.a2.Meta.ID}})
	b.w.bindPolicy(teamPol, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	b.w.bindPolicy(saPol, subject(rolebinding.SubjectServiceAccount, b.sa.Meta.ID))
	h := b.w.start(t)

	// The principal's order wins; the stray credential is outside the team's pools and is narrowed away.
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
	b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)

	r := wantRefused(t, h.route(b.key, "m1"), CodeModelNotAllowed, limits.RungProject)
	if r.PolicyID != p.Meta.ID || r.SubjectID != "project:"+b.proj.Meta.ID {
		t.Fatalf("refusal names %s/%s, want the project policy", r.SubjectID, r.PolicyID)
	}
}

// C19: a disabled policy refuses on its rung, never falls through to a broader one, keeps its dependents and serves again once re-enabled.
func TestRoute_DisabledPolicyRefusesOnItsRungWithoutFallThrough(t *testing.T) {
	b := newBasic()
	off := false
	broad := b.w.policy("broad", policy.Spec{})
	p := b.w.policy("narrow", policy.Spec{Enabled: &off})
	b.w.bindPolicy(broad, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)

	kp := h.keyPlan(b.key)
	if kp == nil {
		t.Fatal("a key under a disabled policy lost its plan")
	}
	if kp.Caller.Refused == nil || kp.Caller.Refused.Code != CodePolicyDisabled || kp.Caller.Refused.Rung != limits.RungProject {
		t.Fatalf("caller refusal = %v, want policy_disabled on the project rung", kp.Caller.Refused)
	}
	wantRefused(t, h.route(b.key, "m1"), CodePolicyDisabled, limits.RungProject)

	on := clone(p)
	on.Spec.Enabled = nil
	apply(h, on)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

func TestRoute_DuplicateBindingRefusesAndFlagsBoth(t *testing.T) {
	b := newBasic()
	p1, p2 := b.w.policy("p1", policy.Spec{}), b.w.policy("p2", policy.Spec{})
	pb1 := b.w.bindPolicy(p1, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	pb2 := b.w.bindPolicy(p2, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)

	wantRefused(t, h.route(b.key, "m1"), CodeDuplicateBinding, limits.RungProject)
	for _, pb := range []string{pb1.Meta.ID, pb2.Meta.ID} {
		if !hasIssue(h.issues(KindPolicyBinding, pb), CodeDuplicateBinding) {
			t.Fatalf("binding %s has no duplicate_binding issue", pb)
		}
	}
	del(h, KindPolicyBinding, pb2.Meta.ID)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
	if hasIssue(h.issues(KindPolicyBinding, pb1.Meta.ID), CodeDuplicateBinding) {
		t.Fatal("duplicate_binding stayed after the second binding went")
	}
}

func TestRoute_GroupSubjectIsInvalid(t *testing.T) {
	b := newBasic()
	p := b.w.policy("p", policy.Spec{Models: []string{"acme/none"}})
	pb := b.w.bindPolicy(p, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"})
	h := b.w.start(t)
	if !hasIssue(h.issues(KindPolicyBinding, pb.Meta.ID), IssueSubjectInvalid) {
		t.Fatal("a group subject raised no subject_invalid")
	}
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

// C36: a missing pool narrows the route and raises ref_missing on the policy; restoring it restores the route.
func TestRoute_MissingPoolNarrowsAndReturns(t *testing.T) {
	b := newBasic()
	poolA := b.w.pool(b.hostA, b.a1)
	poolB := b.w.pool(b.hostB, b.b1)
	p := b.w.policy("p", policy.Spec{PoolIDs: []string{poolA.Meta.ID, poolB.Meta.ID}})
	b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.b1))

	del(h, KindPool, poolA.Meta.ID)
	wantHops(t, h.route(b.key, "m1"), ids(b.b1))
	if !hasIssue(h.issues(KindPolicy, p.Meta.ID), IssueRefMissing) {
		t.Fatal("policy linking a deleted pool has no ref_missing")
	}
	apply(h, poolA)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.b1))
	if hasIssue(h.issues(KindPolicy, p.Meta.ID), IssueRefMissing) {
		t.Fatal("ref_missing stayed after the pool came back")
	}
}

func TestRoute_DeletedBudgetAndRateLimitDropFromTheFold(t *testing.T) {
	b := newBasic()
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	bud := b.w.budget("100")
	p := b.w.policy("p", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}, BudgetIDs: []string{bud.Meta.ID}})
	b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)
	if n := len(h.keyPlan(b.key).Caller.Counters); n != 2 {
		t.Fatalf("caller counters = %d, want a rule and a budget", n)
	}
	del(h, KindBudget, bud.Meta.ID)
	del(h, KindRateLimit, rl.Meta.ID)
	if n := len(h.keyPlan(b.key).Caller.Counters); n != 0 {
		t.Fatalf("caller counters = %d after deleting both links, want 0", n)
	}
	if is := h.issues(KindPolicy, p.Meta.ID); len(is) != 2 || !hasIssue(is, IssueRefMissing) {
		t.Fatalf("issues = %v, want two ref_missing", is)
	}
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

// C20: a disabled tier policy takes its account's credentials out instead of letting them serve unmetered.
func TestRoute_DisabledAccountPolicyTakesItsCredentialsOut(t *testing.T) {
	b := newBasic()
	off := false
	tier := b.w.policy("tier", policy.Spec{Enabled: &off})
	b.w.bindPolicy(tier, subject(policybinding.SubjectAccount, b.acctA.Meta.ID))
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.b1))
	wantRefused(t, h.route(b.key, "m1@host-a"), CodePolicyDisabled, limits.RungAccount)
}

func TestRoute_UpstreamGrantNarrowsSlots(t *testing.T) {
	b := newBasic()
	b.w.model(b.prov, "other")
	tier := b.w.policy("tier", policy.Spec{Models: []string{"acme/other"}})
	b.w.bindPolicy(tier, subject(policybinding.SubjectCredential, b.a1.Meta.ID))
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.a2), ids(b.b1))
}

func TestRoute_UnpricedUnderBudgetIsRefusedAndFlagged(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	bud := b.w.budget("50")
	p := b.w.policy("capped", policy.Spec{BudgetIDs: []string{bud.Meta.ID}})
	pb := b.w.bindPolicy(p, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	h := b.w.start(t)

	r := wantRefused(t, h.route(b.key, "m2"), CodeUnpricedUnderBudget, limits.RungTeam)
	if r.PolicyID != p.Meta.ID {
		t.Fatalf("refusal names policy %s, want the budgeted one", r.PolicyID)
	}
	if !hasIssue(h.issues(KindPolicy, p.Meta.ID), CodeUnpricedUnderBudget) {
		t.Fatal("budgeted policy allowing an unpriced binding has no unpriced_under_budget issue")
	}
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))

	// Without a budget the unpriced binding serves, marked unpriced.
	del(h, KindPolicyBinding, pb.Meta.ID)
	if hop := h.route(b.key, "m2").Hops[0]; hop.Priced || hop.Pricing != nil {
		t.Fatal("an unpriced hop is marked priced")
	}
}

func TestRoute_UpstreamBudgetRefusesUnpricedOnThatSlotOnly(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	b.w.bind(m2, b.hostB)
	b.w.price(m2, b.hostB)
	bud := b.w.budget("50")
	tier := b.w.policy("tier", policy.Spec{BudgetIDs: []string{bud.Meta.ID}})
	b.w.bindPolicy(tier, subject(policybinding.SubjectAccount, b.acctA.Meta.ID))
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m2"), ids(b.b1))
	wantRefused(t, h.route(b.key, "m2@host-a"), CodeUnpricedUnderBudget, limits.RungAccount)
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

// C41: an alias resolves to its model but the ladder still gates it.
func TestRoute_AliasStillPassesTheGate(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Aliases = []string{"m1-latest"}
	b.w.model(b.prov, "other")
	p := b.w.policy("p", policy.Spec{Models: []string{"acme/other"}})
	b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	h := b.w.start(t)
	wantRefused(t, h.route(b.key, "m1-latest"), CodeModelNotAllowed, limits.RungProject)
}

func TestKeyPlan_GraceAndExpiry(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	old := b.key.Spec.KeyHash
	rot := clone(b.key)
	until := time.Now().Add(time.Hour)
	rot.Spec.PreviousKeyHash, rot.Spec.KeyHash, rot.Spec.GraceUntil = old, keyHash("rotated"), &until
	apply(h, rot)
	idx := h.holder.Load().Keys
	now := time.Now()
	if idx.Lookup(rot.Spec.KeyHash, now) == nil || idx.Lookup(old, now) == nil {
		t.Fatal("both hashes should authenticate during grace")
	}
	if idx.Lookup(old, until) != nil {
		t.Fatal("the previous hash authenticated after grace")
	}
	exp := clone(rot)
	past := now.Add(-time.Second)
	exp.Spec.ExpiresAt = &past
	apply(h, exp)
	if h.holder.Load().Keys.Lookup(exp.Spec.KeyHash, now) != nil {
		t.Fatal("an expired key authenticated")
	}
	revoked := clone(exp)
	revoked.Spec.ExpiresAt, revoked.Spec.RevokedAt = nil, &past
	apply(h, revoked)
	if get[KeyPlan](h, revoked.Meta.ID) != nil {
		t.Fatal("a revoked key kept its plan")
	}
}

func TestCells_SharedPerAccountKeyedByValueAndPool(t *testing.T) {
	b := newBasic()
	poolA := b.w.pool(b.hostA, b.a1, b.a2)
	p1 := b.w.policy("p1", policy.Spec{PoolIDs: []string{poolA.Meta.ID}})
	p2 := b.w.policy("p2", policy.Spec{PoolIDs: []string{poolA.Meta.ID}})
	b.w.bindPolicy(p1, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	other := b.w.sa(b.w.project(b.team))
	b.w.bindPolicy(p2, subject(rolebinding.SubjectServiceAccount, other.Meta.ID))
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
	if !hasIssue(h.issues(KindCredential, b.a1.Meta.ID), IssueRefMissing) {
		t.Fatal("no ref_missing on the parked credential")
	}
	wantHops(t, h.route(b.key, "m1"), ids(b.a2), ids(b.b1))
}
