package plan

import (
	"iter"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// A user's service account climbs its owner's personal account, then its own; the personal rung names no team, so usage rows carry neither a team nor a project.
func TestLane_UserServiceAccountClimbsItsOwnersPersonalAccount(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot := b.w.userSA(u)
	k := b.w.saKey(bot)
	h := b.w.start(t)

	cp := h.keyPlan(k).Caller
	got := []string{string(cp.Team().Kind), cp.Team().SubjectID, cp.Team().ID(), cp.Team().Name, cp.Project().SubjectID, cp.Project().ID(), cp.LaneRung().SubjectID, cp.Holder}
	want := []string{string(limits.RungPersonal), "account:" + b.w.personal(u).Meta.ID, "", "", "", "", "account:" + b.w.saAccount(bot).Meta.ID, "serviceaccount:" + bot.Meta.ID}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("personal kind, subject, id, name; project subject, id; lane subject; holder (-want +got):\n%s", diff)
	}
}

// counterKey is what a counter's bucket is: its identity, its atomicity group and the lane it counts under.
type counterKey struct{ ID, Group, Lane string }

func counterKeys(cs []limits.Counter) []counterKey {
	out := make([]counterKey, len(cs))
	for i, c := range cs {
		out[i] = counterKey{c.ID, c.Group, c.Lane}
	}
	return out
}

// remakeBot deletes bot, its account and key, and makes a service account of u again with a key, as a user would to get a fresh allowance.
func remakeBot(h *harness, w *world, u *user.User, bot *serviceaccount.ServiceAccount, k *key.Key) *key.Key {
	h.t.Helper()
	del(h, KindKey, k.Meta.ID)
	del(h, KindAccount, w.saAccount(bot).Meta.ID)
	del(h, KindServiceAccount, bot.Meta.ID)
	again := w.userSA(u)
	apply(h, again)
	apply(h, w.saAccount(again))
	k2 := w.saKey(again)
	apply(h, k2)
	return k2
}

// botKeys yields the keys of a user's two service accounts, then the key of the first one deleted and made again, made only once the two have been checked.
func botKeys(bot, other *key.Key, remake func() *key.Key) iter.Seq2[string, *key.Key] {
	return func(yield func(string, *key.Key) bool) {
		_ = yield("bot", bot) && yield("other bot", other) && yield("bot made again", remake())
	}
}

// A user and every service account they own are one lane for counting: a rate limit or budget on the personal account counts in one bucket for all of them, shared or not, and a service account deleted and made again finds the bucket where it was.
func TestLane_UserAndTheirBotsCountAsOneLaneOnThePersonalAccount(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot, other := b.w.userSA(u), b.w.userSA(u)
	userKey, botKey, otherKey := b.w.userKey(u), b.w.saKey(bot), b.w.saKey(other)
	shared := b.w.rateLimit(rule(ratelimit.MeterRequests, 100, time.Minute))
	perLane := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	bud := b.w.budget("5")
	each := false
	b.w.bindPolicy(b.w.policy("alice-limits", policy.Spec{
		RateLimits: []policy.RateLimitLink{{RateLimitID: shared.Meta.ID}, {RateLimitID: perLane.Meta.ID, Shared: &each}},
		Budgets:    []policy.BudgetLink{{BudgetID: bud.Meta.ID, Shared: &each}},
	}), b.w.personal(u))
	h := b.w.start(t)

	personal := "account:" + b.w.personal(u).Meta.ID
	want := counterKeys(h.keyPlan(userKey).Caller.Counters)
	if len(want) != 3 || want[1].Lane != personal || want[2].Lane != personal || want[0].Group != personal {
		t.Fatalf("the user's counters = %+v, want the shared rule, then the per-lane rule and budget in the personal lane", want)
	}
	for name, k := range botKeys(botKey, otherKey, func() *key.Key { return remakeBot(h, b.w, u, bot, botKey) }) {
		if diff := cmp.Diff(want, counterKeys(h.keyPlan(k).Caller.Counters)); diff != "" {
			t.Errorf("%s: counters (-the user's +got):\n%s", name, diff)
		}
	}
	if got := h.keyPlan(otherKey).Caller.Counters[0].Rung; got != limits.RungPersonal {
		t.Errorf("bot counter rung %s, want personal", got)
	}
}

// A per-lane limit or budget on a provider account the user and their bots reach, by default or through an account shared with the user, counts in one bucket for all of them.
func TestRoute_UserAndTheirBotsCountAsOneLaneUpstream(t *testing.T) {
	each := false
	perLaneTier := func(w *world, a *account.Account) {
		rl := w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
		bud := w.budget("5")
		w.bindPolicy(w.policy("tier-"+seq(), policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID, Shared: &each}}, Budgets: []policy.BudgetLink{{BudgetID: bud.Meta.ID, Shared: &each}}}), a)
	}
	upstream := func(h *harness, k *key.Key, ref string) []counterKey {
		h.t.Helper()
		r := h.route(k, ref)
		if r.Refused != nil || len(r.Hops) == 0 {
			h.t.Fatalf("route %s: refused %v", ref, r.Refused)
		}
		return counterKeys(r.Hops[0].UpstreamCounters(0))
	}

	t.Run("an organization account reached by default", func(t *testing.T) {
		b := newBasic()
		org := b.w.ownedAccount(b.hostA, meta.Global)
		b.w.cred(b.hostA, org, meta.Global)
		perLaneTier(b.w, org)
		u := b.w.user()
		bot, other := b.w.userSA(u), b.w.userSA(u)
		userKey, botKey, otherKey := b.w.userKey(u), b.w.saKey(bot), b.w.saKey(other)
		h := b.w.start(t)

		want := upstream(h, userKey, "m1@host-a")
		if len(want) != 2 || want[0].Lane != "account:"+b.w.personal(u).Meta.ID {
			t.Fatalf("the user's upstream counters = %+v, want the per-lane rule and budget in the personal lane", want)
		}
		for name, k := range botKeys(botKey, otherKey, func() *key.Key { return remakeBot(h, b.w, u, bot, botKey) }) {
			if diff := cmp.Diff(want, upstream(h, k, "m1@host-a")); diff != "" {
				t.Errorf("%s: upstream counters (-the user's +got):\n%s", name, diff)
			}
		}
	})

	t.Run("an account shared with the user", func(t *testing.T) {
		s := newSharing()
		bob := s.w.user()
		shared := s.shareWithUser(bob)
		perLaneTier(s.w, s.real)
		perLaneTier(s.w, shared)
		p := s.w.pool(s.host)
		p.Meta.Owner = userOwner(bob)
		p.Spec.AccountIDs = []string{shared.Meta.ID}
		pol := s.w.policy("bob-pool", policy.Spec{PoolIDs: []string{p.Meta.ID}})
		pol.Meta.Owner = userOwner(bob)
		s.w.bindPolicy(pol, s.w.personal(bob)).Meta.Owner = userOwner(bob)
		bot, other := s.w.userSA(bob), s.w.userSA(bob)
		bobKey, botKey, otherKey := s.w.userKey(bob), s.w.saKey(bot), s.w.saKey(other)
		h := s.w.start(t)

		want := upstream(h, bobKey, "m1")
		if len(want) != 4 {
			t.Fatalf("bob's upstream counters = %+v, want the real account's and the share's per-lane rule and budget", want)
		}
		for name, k := range botKeys(botKey, otherKey, func() *key.Key { return remakeBot(h, s.w, bob, bot, botKey) }) {
			if diff := cmp.Diff(want, upstream(h, k, "m1")); diff != "" {
				t.Errorf("%s: upstream counters (-bob's +got):\n%s", name, diff)
			}
		}
	})
}

// Service accounts a team or a project owns keep a lane each: a per-lane limit above them counts once per service account.
func TestLane_TeamAndProjectServiceAccountsKeepTheirOwnLanes(t *testing.T) {
	b := newBasic()
	inTeam, otherInTeam := b.w.teamSA(b.team), b.w.teamSA(b.team)
	perLane := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	each := false
	b.w.bindPolicy(b.w.policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: perLane.Meta.ID, Shared: &each}}}), b.w.teamAccount(b.team))
	keys := map[*serviceaccount.ServiceAccount]*key.Key{b.sa: b.key, inTeam: b.w.saKey(inTeam), otherInTeam: b.w.saKey(otherInTeam)}
	h := b.w.start(t)

	seen := map[string]bool{}
	for sa, k := range keys {
		cs := h.keyPlan(k).Caller.Counters
		if len(cs) != 1 || cs[0].Lane != "account:"+b.w.saAccount(sa).Meta.ID || seen[cs[0].ID] {
			t.Errorf("%s: counters %+v, want one in its own lane and bucket", sa.Meta.Name, cs)
		}
		if len(cs) == 1 {
			seen[cs[0].ID] = true
		}
	}
}

// A policy on the owner's personal account narrows their bots, and a refusal from it names the personal rung; the user's own requests see it as their lane's.
func TestLane_PersonalPolicyRefusesOnThePersonalRung(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot := b.w.userSA(u)
	userKey, botKey := b.w.userKey(u), b.w.saKey(bot)
	off := false
	pol := b.w.policy("alice-off", policy.Spec{Enabled: &off})
	b.w.bindPolicy(pol, b.w.personal(u))
	h := b.w.start(t)

	for name, tc := range map[string]struct {
		cp   *CallerPlan
		rung limits.Rung
	}{"bot": {h.keyPlan(botKey).Caller, limits.RungPersonal}, "user": {h.keyPlan(userKey).Caller, limits.RungLane}} {
		if r := tc.cp.Refused; r == nil || r.Code != CodePolicyDisabled || r.Rung != tc.rung || r.SubjectID != "account:"+b.w.personal(u).Meta.ID {
			t.Errorf("%s: refused %+v, want policy_disabled on the %s rung of the personal account", name, r, tc.rung)
		}
	}
}

// Without a caller policy a user's service account reaches what its owner owns and the system's, nothing a team, a project or another user owns.
func TestRoute_UserServiceAccountReachesItsOwnersKeysAndSystem(t *testing.T) {
	b := newBasic()
	u, other := b.w.user(), b.w.user()
	own := b.w.cred(b.hostB, b.acctB, meta.Owner{Kind: meta.OwnerUser, ID: u.ID})
	b.w.cred(b.hostA, b.acctA, meta.Owner{Kind: meta.OwnerUser, ID: other.ID})
	b.w.cred(b.hostA, b.acctA, meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID})
	b.w.cred(b.hostA, b.acctA, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	k := b.w.saKey(b.w.userSA(u))
	h := b.w.start(t)

	wantHops(t, h.route(k, "m1"), ids(own), ids(b.a1), ids(b.a2), ids(b.b1))
}

// Turning the owner's personal accounts off closes their service accounts' lanes with personal_account_off, on the lane's account and on the keys; turning them on again opens them.
func TestPersonalAccountOff_ClosesTheUsersServiceAccounts(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot := b.w.userSA(u)
	k := b.w.saKey(bot)
	h := b.w.start(t)
	if r := h.route(k, "m1"); r.Refused != nil {
		t.Fatalf("before: %v", r.Refused)
	}

	apply(h, withPersonalAccount(u, settings.PersonalAccountsOff))
	if r := h.keyPlan(k).Caller.Refused; r == nil || r.Code != CodePersonalAccountOff {
		t.Errorf("bot key: refused %+v, want %s", r, CodePersonalAccountOff)
	}
	h.wantIssue(KindAccount, b.w.saAccount(bot).Meta.ID, IssuePersonalAccountOff, true)
	h.wantIssue(KindKey, k.Meta.ID, IssuePersonalAccountOff, true)
	if cp := LanePlan(h.e.View(), b.w.saAccount(bot).Meta.ID); cp == nil || cp.Refused == nil || cp.Refused.Code != CodePersonalAccountOff {
		t.Errorf("lane plan = %+v, want refused with %s", cp, CodePersonalAccountOff)
	}
	if r := h.keyPlan(b.key).Caller.Refused; r != nil {
		t.Errorf("a project's service account refused: %v", r)
	}

	apply(h, withPersonalAccount(u, settings.PersonalAccountsOn))
	if r := h.route(k, "m1"); r.Refused != nil {
		t.Errorf("after turning it on: %v", r.Refused)
	}
	h.wantIssue(KindAccount, b.w.saAccount(bot).Meta.ID, IssuePersonalAccountOff, false)
}

// A disabled owner refuses their service accounts' lanes; a deleted one leaves the lane's account with an issue naming the missing user.
func TestLane_UserServiceAccountFollowsItsOwner(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot := b.w.userSA(u)
	k := b.w.saKey(bot)
	h := b.w.start(t)

	disabled := clone(u)
	disabled.Disabled = true
	apply(h, disabled)
	if r := h.keyPlan(k).Caller.Refused; r == nil || r.Code != CodeSubjectDisabled || r.Rung != limits.RungPersonal {
		t.Errorf("owner disabled: refused %+v, want subject_disabled on the personal rung", r)
	}

	del(h, KindUser, u.ID)
	botAcct := b.w.saAccount(bot)
	if i := issueAt(h.issues(KindAccount, botAcct.Meta.ID), IssueRefMissing, "metadata.owner"); i == nil || i.Target != (engine.Ref{Kind: KindUser, ID: u.ID}) {
		t.Errorf("owner gone: issue = %+v, want the deleted user as the owner", i)
	}
}

// An account a user owns and their service account holds is that service account's: the user's key cannot run in it, the user's token cannot switch into it, and the user's member lanes and own lane are untouched by it.
func TestLane_UserServiceAccountAccountsAreNotTheUsers(t *testing.T) {
	s := newSharing()
	bot := s.w.userSA(s.alice)
	botShare := s.w.share(s.real, account.Receiver{Kind: account.ReceiverServiceAccount, ID: bot.Meta.ID}, userOwner(s.alice))
	aliceKey := s.w.userKey(s.alice)
	h := s.w.start(t)
	root, kp := h.holder.Load(), h.keyPlan(aliceKey)

	for name, ref := range map[string]string{"the bot's lane": s.w.saAccount(bot).Meta.ID, "the bot's share": botShare.Meta.ID} {
		if _, r := root.LaneFor(kp, ref); r == nil || r.Code != CodeAccountNotHeld {
			t.Errorf("alice's key into %s: refusal %+v, want %s", name, r, CodeAccountNotHeld)
		}
		if _, r := root.TokenLane(s.alice.ID, "", ref); r == nil || r.Code != CodeAccountNotHeld {
			t.Errorf("alice's token into %s: refusal %+v, want %s", name, r, CodeAccountNotHeld)
		}
	}
	if cp := LanePlan(h.e.View(), botShare.Meta.ID); cp == nil || !cp.HeldBy("serviceaccount:"+bot.Meta.ID) || cp.HeldBy("user:"+s.alice.ID) {
		t.Errorf("the bot's share is held by the user or not by the bot: %+v", cp)
	}
	if got := kp.Caller.Lane.Meta.ID; got != s.w.personal(s.alice).Meta.ID {
		t.Errorf("alice's key runs in %s, want her personal account", got)
	}
}
