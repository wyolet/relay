package plan

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// A member of a team climbs the team's account and its own; a member of a project the team's, the project's and its own; a personal account climbs nothing above itself.
func TestLane_RungsFollowTheAccountsPlace(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	inTeam := b.w.member(u, meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID})
	inProject := b.w.member(u, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	teamKey, projectKey, personalKey := b.w.keyIn(u, inTeam), b.w.keyIn(u, inProject), b.w.userKey(u)
	h := b.w.start(t)

	for _, tc := range []struct {
		name                string
		key                 *CallerPlan
		team, project, lane string
	}{
		{"team member", h.keyPlan(teamKey).Caller, "account:" + b.w.teamAccount(b.team).Meta.ID, "", "account:" + inTeam.Meta.ID},
		{"project member", h.keyPlan(projectKey).Caller, "account:" + b.w.teamAccount(b.team).Meta.ID, "account:" + b.w.projectAccount(b.proj).Meta.ID, "account:" + inProject.Meta.ID},
		{"personal", h.keyPlan(personalKey).Caller, "", "", "account:" + b.w.personal(u).Meta.ID},
	} {
		if cp := tc.key; cp.Team().SubjectID != tc.team || cp.Project().SubjectID != tc.project || cp.LaneRung().SubjectID != tc.lane || cp.Holder != "user:"+u.ID {
			t.Errorf("%s: rungs %q / %q / %q holder %q", tc.name, cp.Team().SubjectID, cp.Project().SubjectID, cp.LaneRung().SubjectID, cp.Holder)
		}
	}
	if cp := h.keyPlan(teamKey).Caller; cp.Team().ID() != b.team.Meta.ID || cp.Project().ID() != "" {
		t.Errorf("team member scope = team %q project %q", cp.Team().ID(), cp.Project().ID())
	}
}

// A link with shared off counts once per lane under the bound account; a shared one once for the account, whichever lane the request runs in.
func TestLane_SharedAndPerLaneCallerCounters(t *testing.T) {
	b := newBasic()
	other := b.w.sa(b.w.project(b.team))
	otherKey := b.w.saKey(other)
	shared := b.w.rateLimit(rule(ratelimit.MeterRequests, 100, time.Minute))
	perLane := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	each := false
	b.w.bindPolicy(b.w.policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: shared.Meta.ID}, {RateLimitID: perLane.Meta.ID, Shared: &each}}}), b.w.teamAccount(b.team))
	h := b.w.start(t)

	c1, c2 := h.keyPlan(b.key).Caller.Counters, h.keyPlan(otherKey).Caller.Counters
	if len(c1) != 2 || len(c2) != 2 {
		t.Fatalf("counters %d / %d, want the shared and the per-lane rule", len(c1), len(c2))
	}
	if c1[0].ID != c2[0].ID || c1[0].Lane != "" {
		t.Errorf("shared rule: %s / %s lane %q, want one bucket for the team", c1[0].ID, c2[0].ID, c1[0].Lane)
	}
	if c1[1].ID == c2[1].ID || c1[1].Lane != "account:"+b.w.saAccount(b.sa).Meta.ID {
		t.Errorf("per-lane rule: %s / %s lane %q, want a bucket per lane", c1[1].ID, c2[1].ID, c1[1].Lane)
	}
	if c1[1].Group != c1[0].Group {
		t.Errorf("per-lane rule grouped under %s, want the team's %s", c1[1].Group, c1[0].Group)
	}
}

// On a provider account, a shared link is the org-wide tier and one with shared off is counted per caller lane.
func TestLane_PerLaneUpstreamCounters(t *testing.T) {
	b := newBasic()
	other := b.w.sa(b.w.project(b.team))
	otherKey := b.w.saKey(other)
	org := b.w.rateLimit(rule(ratelimit.MeterRequests, 1000, time.Minute))
	perLane := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	each := false
	b.w.bindPolicy(b.w.policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: org.Meta.ID}, {RateLimitID: perLane.Meta.ID, Shared: &each}}}), b.acctA)
	h := b.w.start(t)

	up := func(r *Route) []limits.Counter { return r.Hops[0].UpstreamCounters(0) }
	u1, u2 := up(h.route(b.key, "m1@host-a")), up(h.route(otherKey, "m1@host-a"))
	if len(u1) != 2 || len(u2) != 2 {
		t.Fatalf("upstream counters %d / %d, want two", len(u1), len(u2))
	}
	if u1[0].ID != u2[0].ID || u1[0].Lane != "" {
		t.Errorf("org-wide rule counts per lane: %s / %s", u1[0].ID, u2[0].ID)
	}
	if u1[1].ID == u2[1].ID || u1[1].Lane == "" || u1[1].Group != "account:"+b.acctA.Meta.ID {
		t.Errorf("per-lane tier rule: %s / %s lane %q group %s", u1[1].ID, u2[1].ID, u1[1].Lane, u1[1].Group)
	}
}

// A provider account used as a lane reaches its own keys alone; its policy applies as the upstream rung, never twice.
func TestLane_ProviderAccountReachesOnlyItsOwnKeys(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	mine := b.w.account(b.hostA)
	mine.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	cred := b.w.cred(b.hostA, mine, mine.Meta.Owner)
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	b.w.bindPolicy(b.w.policy("my-tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), mine)
	k := b.w.keyIn(u, mine)
	h := b.w.start(t)

	cp := h.keyPlan(k).Caller
	if cp.Refused != nil || !cp.Lane.IsProvider() || !cp.HeldBy("user:"+u.ID) || cp.HeldBy("user:"+meta.NewID()) {
		t.Fatalf("provider lane refused %v, held by its owner %v", cp.Refused, cp.HeldBy("user:"+u.ID))
	}
	r := h.route(k, "m1")
	wantHops(t, r, ids(cred))
	if hop := r.Hops[0]; hop.CallerN != 0 || len(hop.Counters[0]) != 1 || hop.Counters[0][0].Rung != limits.RungAccount {
		t.Fatalf("counters = %+v (caller %d), want the account's one rule", hop.Counters[0], hop.CallerN)
	}
}

// A closed member account refuses its keys; reopened, they serve again.
func TestLane_ClosedAccountRefuses(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	m := b.w.member(u, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	m.Spec.Closed = true
	k := b.w.keyIn(u, m)
	h := b.w.start(t)

	_ = wantRefused(t, h.route(k, "m1"), CodeAccountClosed, limits.RungLane)
	open := clone(m)
	open.Spec.Closed = false
	apply(h, open)
	wantHops(t, h.route(k, "m1"), ids(b.a1), ids(b.a2), ids(b.b1))
}

// A key whose lane its principal does not hold serves nothing, with account_not_held, rather than running in someone else's account.
func TestLane_KeyInAnAccountItsPrincipalDoesNotHold(t *testing.T) {
	b := newBasic()
	alice, bob := b.w.user(), b.w.user()
	k := b.w.keyIn(alice, b.w.personal(bob))
	h := b.w.start(t)
	_ = wantRefused(t, h.route(k, "m1"), CodeAccountNotHeld, limits.RungLane)
}

// A key outlives the membership it was made under, so it is refused in a member account opened on first use, in a provider account its user does not own, and in one its user stops owning.
func TestLane_KeyLandsOnlyWhereRelayRechecksMembership(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	firstUse := b.w.member(u, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	firstUse.FollowsRoleBindings = false
	teamOrg := b.w.account(b.hostA)
	teamOrg.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	own := b.w.account(b.hostA)
	own.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	b.w.cred(b.hostA, own, own.Meta.Owner)
	inFirstUse, inTeamOrg, inOwn := b.w.keyIn(u, firstUse), b.w.keyIn(u, teamOrg), b.w.keyIn(u, own)
	h := b.w.start(t)

	_ = wantRefused(t, h.route(inFirstUse, "m1"), CodeAccountNotHeld, limits.RungLane)
	_ = wantRefused(t, h.route(inTeamOrg, "m1"), CodeAccountNotHeld, limits.RungLane)
	if cp := h.keyPlan(inOwn).Caller; cp.Refused != nil {
		t.Fatalf("a key in its user's own provider account refused: %v", cp.Refused)
	}
	moved := clone(own)
	moved.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	apply(h, moved)
	_ = wantRefused(t, h.route(inOwn, "m1"), CodeAccountNotHeld, limits.RungLane)
}

// A key saved without a lane runs in its principal's own account.
func TestLane_KeyWithoutAnAccountRunsInItsPrincipalsOwn(t *testing.T) {
	b := newBasic()
	b.key.Spec.AccountID = ""
	h := b.w.start(t)
	if cp := h.keyPlan(b.key).Caller; cp.Lane.Meta.ID != b.w.saAccount(b.sa).Meta.ID {
		t.Fatalf("lane = %s, want the service account's account", cp.Lane.Meta.Name)
	}
}

// The nearest account on the ladder that sets payloadLogging decides, lane first; with none set the key's flag does.
func TestLane_PayloadLoggingNearestAccountDecides(t *testing.T) {
	b := newBasic()
	team := b.w.teamAccount(b.team)
	team.Spec.PayloadLogging = account.PayloadLoggingOn
	h := b.w.start(t)
	if cp := h.keyPlan(b.key).Caller; !cp.CapturesPayload(false) {
		t.Fatal("the team account's on did not reach its lane")
	}
	lane := clone(b.w.saAccount(b.sa))
	lane.Spec.PayloadLogging = account.PayloadLoggingOff
	apply(h, lane)
	if cp := h.keyPlan(b.key).Caller; cp.CapturesPayload(true) {
		t.Fatal("the lane's off lost to the team's on and the key's flag")
	}
	u := b.w.user()
	uk := b.w.userKey(u)
	apply(h, b.w.personal(u))
	apply(h, u)
	apply(h, uk)
	if cp := h.keyPlan(uk).Caller; !cp.CapturesPayload(true) || cp.CapturesPayload(false) {
		t.Fatal("with no account setting it, the key's flag does not decide")
	}
}

// A request may run in another account its key's principal holds, named by id or name; one it does not hold is refused the same as one that does not exist.
func TestLane_LaneForResolvesHeldAccountsOnly(t *testing.T) {
	b := newBasic()
	u, other := b.w.user(), b.w.user()
	inProject := b.w.member(u, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	k := b.w.userKey(u)
	h := b.w.start(t)
	root, kp := h.holder.Load(), h.keyPlan(k)

	if cp, r := root.LaneFor(kp, ""); r != nil || cp != kp.Caller {
		t.Fatalf("default lane = %v, %v", cp, r)
	}
	for _, ref := range []string{inProject.Meta.ID, inProject.Meta.Name} {
		if cp, r := root.LaneFor(kp, ref); r != nil || cp.Lane.Meta.ID != inProject.Meta.ID {
			t.Fatalf("lane %s = %v, %v; want the member account", ref, cp, r)
		}
	}
	for _, ref := range []string{b.w.personal(other).Meta.ID, "no-such-account"} {
		if _, r := root.LaneFor(kp, ref); r == nil || r.Code != CodeAccountNotHeld {
			t.Fatalf("lane %s refusal = %v, want account_not_held", ref, r)
		}
	}
	if cp, r := root.MemberLane(u.ID, b.proj.Meta.ID); r != nil || cp.Lane.Meta.ID != inProject.Meta.ID {
		t.Fatalf("member lane = %v, %v", cp, r)
	}
}

// A token scoped to a project switches only into accounts inside that project; one without a project into any account its user holds.
func TestLane_TokenSwitchStaysInItsProject(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	inProject := b.w.member(u, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	inTeam := b.w.member(u, meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID})
	personal := b.w.personal(u)
	h := b.w.start(t)
	root, proj := h.holder.Load(), b.proj.Meta.ID

	if cp, r := root.TokenLane(u.ID, proj, inProject.Meta.Name); r != nil || cp.Lane.Meta.ID != inProject.Meta.ID {
		t.Fatalf("project token into its member account = %v, %v", cp, r)
	}
	for name, ref := range map[string]string{"team member account": inTeam.Meta.ID, "personal account": personal.Meta.ID} {
		if _, r := root.TokenLane(u.ID, proj, ref); r == nil || r.Code != CodeAccountNotHeld {
			t.Errorf("project token into its %s: refusal = %v, want account_not_held", name, r)
		}
		if cp, r := root.TokenLane(u.ID, "", ref); r != nil || cp.Lane.Meta.ID != ref {
			t.Errorf("token without a project into its %s = %v, %v", name, cp, r)
		}
	}
}

// A key switches by the rules a key's own account obeys: never into a member account opened on first use, a team's account, or a provider account its user does not own. A token switches into any account its user holds. A provider lane of a disabled user is refused, since its plan reads no holder.
func TestLane_SwitchFollowsTheKeyRules(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	firstUse := b.w.member(u, meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID})
	firstUse.FollowsRoleBindings = false
	teamOrg := b.w.account(b.hostA)
	teamOrg.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	own := b.w.account(b.hostA)
	own.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	k := b.w.userKey(u)
	h := b.w.start(t)
	root, kp := h.holder.Load(), h.keyPlan(k)

	for name, ref := range map[string]string{"first-use member": firstUse.Meta.ID, "team account": b.w.teamAccount(b.team).Meta.ID, "team's provider account": teamOrg.Meta.ID} {
		if _, r := root.LaneFor(kp, ref); r == nil || r.Code != CodeAccountNotHeld {
			t.Errorf("key into %s: refusal = %v, want account_not_held", name, r)
		}
	}
	if cp, r := root.LaneFor(kp, own.Meta.Name); r != nil || cp.Refused != nil || cp.Lane.Meta.ID != own.Meta.ID {
		t.Fatalf("key into its user's provider account = %v, %v", cp, r)
	}
	if cp, r := root.TokenLane(u.ID, "", firstUse.Meta.ID); r != nil || cp.Lane.Meta.ID != firstUse.Meta.ID {
		t.Fatalf("token into first-use member = %v, %v", cp, r)
	}
	if _, r := root.TokenLane(u.ID, "", teamOrg.Meta.ID); r == nil || r.Code != CodeAccountNotHeld {
		t.Fatalf("token into the team's provider account: refusal = %v", r)
	}

	off := *u
	off.Disabled = true
	apply(h, &off)
	root = h.holder.Load()
	for name, resolve := range map[string]func() (*CallerPlan, *Refusal){
		"key":   func() (*CallerPlan, *Refusal) { return root.LaneFor(h.keyPlan(k), own.Meta.ID) },
		"token": func() (*CallerPlan, *Refusal) { return root.TokenLane(u.ID, "", own.Meta.ID) },
	} {
		cp, r := resolve()
		if r != nil || cp.Refused == nil || cp.Refused.Code != CodeSubjectDisabled || cp.Refused.Rung != limits.RungLane {
			t.Errorf("%s into a disabled user's provider account = %v, %v; want a lane refusing subject_disabled", name, cp, r)
		}
	}
}

func pendingFor(h *harness, place string) *ManagedAccount {
	for _, m := range Pending(h.e.View()) {
		if m.Key == place {
			return m
		}
	}
	return nil
}

// The plans call for every caller account the tenancy rows imply: a team without one is pending creation, a member account whose last role grant went is pending closing, one granted again pending reopening.
func TestManagedAccounts_PendingCreateCloseReopen(t *testing.T) {
	b := newBasic()
	team := b.w.team()
	b.w.accounts = b.w.accounts[:len(b.w.accounts)-1] // as if relay had not made its account yet
	u := b.w.user()
	scope := meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}
	member := b.w.member(u, scope)
	grant := b.w.rbs[len(b.w.rbs)-1]
	h := b.w.start(t)

	if m := pendingFor(h, "team:"+team.Meta.ID); m == nil || m.Action() != AccountCreate || m.Want.DerivedKind() != account.KindTeam {
		t.Fatalf("team place = %+v, want pending creation", m)
	}
	if m := pendingFor(h, member.ManagedKey()); m != nil {
		t.Fatalf("a granted, open member account is pending %v", m.Action())
	}
	del(h, KindRoleBinding, grant.Meta.ID)
	if m := pendingFor(h, member.ManagedKey()); m == nil || m.Action() != AccountClose {
		t.Fatalf("member place after the grant went = %+v, want pending closing", m)
	}
	closed := clone(member)
	closed.Spec.Closed = true
	apply(h, closed)
	if m := pendingFor(h, member.ManagedKey()); m != nil {
		t.Fatalf("a closed, ungranted member account is pending %v", m.Action())
	}
	apply(h, grant)
	if m := pendingFor(h, member.ManagedKey()); m == nil || m.Action() != AccountReopen {
		t.Fatalf("member place granted again = %+v, want pending reopening", m)
	}

	// A user granted a role in a project with no account there yet is pending a member account.
	newcomer := b.w.user()
	apply(h, newcomer)
	apply(h, &rolebinding.RoleBinding{
		Meta: md("rb-"+seq(), scope),
		Spec: rolebinding.Spec{RoleID: meta.NewID(), Scope: scope, Subjects: []rolebinding.Subject{userSubject(newcomer.ID)}},
	})
	if m := pendingFor(h, account.MemberKey(scope, newcomer.ID)); m == nil || m.Action() != AccountCreate || !m.Want.FollowsRoleBindings {
		t.Fatalf("newcomer's member place = %+v, want pending creation following role bindings", m)
	}
}
