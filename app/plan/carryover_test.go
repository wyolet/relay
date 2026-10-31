package plan

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
)

// C15: /v1/models never lists what resolution refuses, and lists everything it serves.
func TestCarryOver_ListingMatchesResolve(t *testing.T) {
	b := newBasic()
	w := b.w
	gone := w.model(b.prov, "gone")
	w.bind(gone, b.hostA)
	off := false
	gone.Spec.Enabled = &off
	old := w.model(b.prov, "old")
	w.bind(old, b.hostA)
	w.price(old, b.hostA)
	old.Spec.Deprecation = &model.Deprecation{Status: model.DeprecationDeprecated}
	unpriced := w.model(b.prov, "unpriced")
	w.bind(unpriced, b.hostB)
	hidden := w.model(b.prov, "hidden")
	w.bind(hidden, b.hostB)
	w.price(hidden, b.hostB)
	w.model(b.prov, "unbound")
	tierOnly := w.model(b.prov, "tier-only")
	w.bind(tierOnly, b.hostA)
	w.price(tierOnly, b.hostA)

	bud := w.budget("10")
	teamPol := w.policy("team", policy.Spec{BudgetIDs: []string{bud.Meta.ID}})
	projPol := w.policy("proj", policy.Spec{Models: []string{"acme"}, IncludeDeprecated: true})
	saPol := w.policy("sa", policy.Spec{Models: []string{"acme/m1", "acme/old", "acme/unpriced", "acme/tier-only", "acme/gone"}})
	tier := w.policy("tier-a", policy.Spec{Models: []string{"acme/m1", "acme/old"}})
	w.bindPolicy(teamPol, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	w.bindPolicy(projPol, subject(policybinding.SubjectProject, b.proj.Meta.ID))
	w.bindPolicy(saPol, subject(rolebinding.SubjectServiceAccount, b.sa.Meta.ID))
	w.bindPolicy(tier, subject(policybinding.SubjectAccount, b.acctA.Meta.ID))
	h := w.start(t)

	rt := h.keyPlan(b.key).Caller.Routes
	var listed []string
	for _, m := range rt.Models() {
		listed = append(listed, m.Meta.Name)
	}
	for _, m := range w.models {
		served := len(rt.Get(m.Spec.Snapshots[0].Name).Hops) > 0
		if served != slices.Contains(listed, m.Meta.Name) {
			t.Errorf("%s: listed=%v, resolve serves=%v", m.Meta.Name, !served, served)
		}
	}
	// m1 serves; old is deprecated but every rung admits it; unpriced (budget), hidden and gone (principal grant), tier-only (account grant) and unbound do not.
	if want := []string{"m1", "old"}; !slices.Equal(listed, want) {
		t.Errorf("listed %v, want %v", listed, want)
	}
}

// C16: a keyless host is reached only through a pool linked on purpose, never by a wildcard grant or by ownership.
func TestCarryOver_KeylessHostNeedsAnExplicitPool(t *testing.T) {
	b := newBasic()
	w := b.w
	local := w.host("local")
	local.Spec.NoAuth = true
	m3 := w.model(b.prov, "m3")
	w.bind(m3, local)
	h := w.start(t)
	wantRefused(t, h.route(b.key, "m3"), CodeNoCredentials, "")

	wild := w.policy("wild", policy.Spec{Models: []string{"acme"}})
	apply(h, wild)
	apply(h, w.bindPolicy(wild, subject(policybinding.SubjectProject, b.proj.Meta.ID)))
	wantRefused(t, h.route(b.key, "m3"), CodeNoCredentials, "")

	lp := w.pool(local)
	apply(h, lp)
	explicit := clone(wild)
	explicit.Spec.PoolIDs = []string{lp.Meta.ID}
	apply(h, explicit)
	r := h.route(b.key, "m3")
	if r.Refused != nil || len(r.Hops) != 1 || len(r.Hops[0].Slots) != 1 {
		t.Fatalf("explicit keyless pool route = %+v", r)
	}
	s := r.Hops[0].Slots[0]
	if s.Credential.Meta.ID != hostkey.AnonIDPrefix+local.Meta.ID || s.Breaker != nil || s.Account != nil {
		t.Fatalf("keyless slot = %s breaker=%v account=%v, want the anonymous key without breaker", s.Credential.Meta.ID, s.Breaker, s.Account)
	}
}

// C17: deprecated grants, snapshot-gated bindings, disabled and unknown models, hosts without a base URL.
func TestCarryOver_DeprecatedGrantMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  *policy.Spec
		serve bool
	}{
		{"no caller policy", nil, false},
		{"wildcard", &policy.Spec{Models: []string{"acme"}}, false},
		{"wildcard with includeDeprecated", &policy.Spec{Models: []string{"acme"}, IncludeDeprecated: true}, true},
		{"named", &policy.Spec{Models: []string{"acme/m1"}}, true},
		{"empty list with includeDeprecated", &policy.Spec{IncludeDeprecated: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBasic()
			b.m1.Spec.Deprecation = &model.Deprecation{Status: model.DeprecationSunset}
			if tc.spec != nil {
				b.w.bindPolicy(b.w.policy("p", *tc.spec), subject(policybinding.SubjectProject, b.proj.Meta.ID))
			}
			r := b.w.start(t).route(b.key, "m1")
			if served := r.Refused == nil; served != tc.serve {
				t.Fatalf("served = %v, want %v (%v)", served, tc.serve, r.Refused)
			}
			if !tc.serve && r.Refused.Code != CodeModelNotAllowed {
				t.Fatalf("refusal = %v, want model_not_allowed", r.Refused)
			}
		})
	}
}

func TestCarryOver_BindingServesOnlyItsSnapshots(t *testing.T) {
	b := newBasic()
	b.m1.Spec.Snapshots = append(b.m1.Spec.Snapshots, model.Snapshot{Name: "m1-2026"})
	for _, bd := range b.w.bindings {
		if bd.Spec.HostID == b.hostB.Meta.ID {
			bd.Spec.Snapshots = []string{"m1"}
		}
	}
	h := b.w.start(t)
	wantHops(t, h.route(b.key, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
	wantHops(t, h.route(b.key, "m1-2026"), ids(b.a1), ids(b.a2))
	wantRefused(t, h.route(b.key, "m1-2026@host-b"), CodeNoBinding, "")
}

func TestCarryOver_DisabledUnknownAndUnroutable(t *testing.T) {
	b := newBasic()
	off := false
	gone := b.w.model(b.prov, "gone")
	gone.Spec.Enabled = &off
	b.w.bind(gone, b.hostA)
	dark := b.w.host("dark")
	dark.Spec.BaseURL = ""
	m4 := b.w.model(b.prov, "m4")
	b.w.bind(m4, dark)
	b.w.pool(dark, b.w.cred(dark, nil, system))
	h := b.w.start(t)
	wantRefused(t, h.route(b.key, "gone"), CodeModelDisabled, "")
	wantRefused(t, h.route(b.key, "nope"), CodeModelNotFound, "")
	wantRefused(t, h.route(b.key, ""), CodeModelNotFound, "")
	wantRefused(t, h.route(b.key, "m4"), CodeNoBinding, "")
}

// C29: a grant that can never match narrows to nothing and is reported, never read as "no list".
func TestCarryOver_UnmatchableGrantNarrowsToNothing(t *testing.T) {
	for _, ref := range []string{"group:wr/frontier", "a/b/c/d@@"} {
		t.Run(ref, func(t *testing.T) {
			b := newBasic()
			p := b.w.policy("p", policy.Spec{Models: []string{ref}})
			b.w.bindPolicy(p, subject(policybinding.SubjectProject, b.proj.Meta.ID))
			h := b.w.start(t)
			wantRefused(t, h.route(b.key, "m1"), CodeModelNotAllowed, limits.RungProject)
			is := h.issues(KindPolicy, p.Meta.ID)
			if !hasIssue(is, IssueModelGroupUnsupported) && !hasIssue(is, IssueModelRefInvalid) {
				t.Fatalf("issues = %v, want the unmatchable ref reported", is)
			}
		})
	}
}

// C1: every model a scoped upstream rule matches gets its own counter; an unscoped rule counts once for all.
func TestCounters_ScopedUpstreamRulesAreDistinctPerModel(t *testing.T) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	b.w.price(m2, b.hostA)
	scoped := b.w.rateLimit(rule(ratelimit.MeterTokens, 1000, time.Minute))
	whole := b.w.rateLimit(rule(ratelimit.MeterRequests, 60, time.Minute))
	tier := b.w.policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{
		{RateLimitID: scoped.Meta.ID, Models: []string{"acme/m1"}},
		{RateLimitID: scoped.Meta.ID, Models: []string{"acme/m2"}},
		{RateLimitID: whole.Meta.ID},
	}})
	b.w.bindPolicy(tier, subject(policybinding.SubjectAccount, b.acctA.Meta.ID))
	h := b.w.start(t)

	c1 := h.route(b.key, "m1@host-a").Hops[0].Counters[0]
	c2 := h.route(b.key, "m2@host-a").Hops[0].Counters[0]
	if len(c1) != 2 || len(c2) != 2 {
		t.Fatalf("counters m1=%d m2=%d, want the whole rule and one scoped rule each", len(c1), len(c2))
	}
	if c1[0].ID != c2[0].ID {
		t.Error("the unscoped rule counts per model")
	}
	if c1[1].ID == c2[1].ID || c1[1].ModelID != b.m1.Meta.ID {
		t.Errorf("scoped rule shares a bucket across models: %s / %s", c1[1].ID, c2[1].ID)
	}
	for _, c := range append(c1, c2...) {
		if c.Rung != limits.RungAccount || c.Group != "account:"+b.acctA.Meta.ID || c.SubjectID != "account:"+b.acctA.Meta.ID {
			t.Errorf("upstream counter %s: rung %s group %s subject %s", c.ID, c.Rung, c.Group, c.SubjectID)
		}
	}
	// The other credential of the account counts in the same buckets.
	other := h.route(b.key, "m1@host-a").Hops[1].Counters[0]
	if !slices.EqualFunc(c1, other, func(a, b limits.Counter) bool { return a.ID == b.ID }) {
		t.Error("credentials of one account count tier rules separately")
	}
}

// Caller counters count per (rule, policy, bound subject): a reused policy gives each subject its own bucket, a team binding one bucket for the whole team.
func TestCounters_CallerIdentityPerBoundSubject(t *testing.T) {
	b := newBasic()
	w := b.w
	rl := w.rateLimit(rule(ratelimit.MeterRequests, 100, time.Minute))
	shared := w.policy("shared", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	teamRL := w.rateLimit(rule(ratelimit.MeterRequests, 1000, time.Minute))
	teamPol := w.policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: teamRL.Meta.ID}}})
	proj2 := w.project(b.team)
	sa2 := w.sa(proj2)
	key2 := w.saKey(sa2)
	keyB := w.saKey(b.sa)
	w.bindPolicy(shared, subject(policybinding.SubjectProject, b.proj.Meta.ID), subject(policybinding.SubjectProject, proj2.Meta.ID))
	w.bindPolicy(teamPol, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	h := w.start(t)

	k1, k2, kB := h.keyPlan(b.key), h.keyPlan(key2), h.keyPlan(keyB)
	if k1.Caller != kB.Caller {
		t.Fatal("two keys of one principal hold different caller plans")
	}
	c1, c2 := k1.Caller.Counters, k2.Caller.Counters
	if len(c1) != 2 || len(c2) != 2 {
		t.Fatalf("counters %d / %d, want team and project rules", len(c1), len(c2))
	}
	if c1[0].ID != c2[0].ID || c1[0].Rung != limits.RungTeam {
		t.Errorf("team rule not shared by the team: %s / %s", c1[0].ID, c2[0].ID)
	}
	if c1[1].ID == c2[1].ID {
		t.Error("one policy bound to two projects shares a bucket")
	}
	for _, c := range append(c1, c2...) {
		if c.Group != "team:"+b.team.Meta.ID {
			t.Errorf("caller counter %s grouped under %s, want the team", c.ID, c.Group)
		}
	}
	// Route counters lead with the caller's.
	rc := h.route(b.key, "m1").Hops[0].Counters[0]
	if len(rc) < 2 || rc[0].ID != c1[0].ID || rc[1].ID != c1[1].ID {
		t.Error("route counters do not start with the caller's")
	}
}

// A keyless host's pool is one anonymous slot whatever the pool lists.
func TestPoolPlan_KeylessPoolIgnoresCredentials(t *testing.T) {
	w := &world{}
	local := w.host("local")
	local.Spec.NoAuth = true
	c := w.cred(local, nil, system)
	p := w.pool(local, c)
	h := w.start(t)
	pp := get[PoolPlan](h, p.Meta.ID)
	if len(pp.Slots) != 1 || pp.Slots[0].Credential.Meta.ID != hostkey.AnonIDPrefix+local.Meta.ID {
		t.Fatalf("keyless pool slots = %v", pp.Slots)
	}
	if !hasIssue(h.issues(KindPool, p.Meta.ID), IssueCredentialsIgnored) {
		t.Fatal("a keyless pool listing credentials raised nothing")
	}
}
