package plan

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/settings"
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

// A limit shared on the owner's personal account counts once for the user and all their bots; one counted each counts per lane, under the personal account's group.
func TestLane_PersonalLimitsCountOnceAcrossTheUserAndTheirBots(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	bot, other := b.w.userSA(u), b.w.userSA(u)
	userKey, botKey, otherKey := b.w.userKey(u), b.w.saKey(bot), b.w.saKey(other)
	shared := b.w.rateLimit(rule(ratelimit.MeterRequests, 100, time.Minute))
	perLane := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	each := false
	b.w.bindPolicy(b.w.policy("alice-limits", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: shared.Meta.ID}, {RateLimitID: perLane.Meta.ID, Shared: &each}}}), b.w.personal(u))
	h := b.w.start(t)

	personal := "account:" + b.w.personal(u).Meta.ID
	c := map[string][]limits.Counter{"user": h.keyPlan(userKey).Caller.Counters, "bot": h.keyPlan(botKey).Caller.Counters, "other bot": h.keyPlan(otherKey).Caller.Counters}
	for name, cs := range c {
		if len(cs) != 2 {
			t.Fatalf("%s: %d counters, want the shared and the per-lane rule", name, len(cs))
		}
		if cs[0].Group != personal || cs[1].Group != personal {
			t.Errorf("%s: groups %s / %s, want the personal account's %s", name, cs[0].Group, cs[1].Group, personal)
		}
	}
	if c["user"][0].ID != c["bot"][0].ID || c["bot"][0].ID != c["other bot"][0].ID {
		t.Errorf("shared rule: %s / %s / %s, want one bucket for the user and their bots", c["user"][0].ID, c["bot"][0].ID, c["other bot"][0].ID)
	}
	if c["user"][1].ID == c["bot"][1].ID || c["bot"][1].ID == c["other bot"][1].ID || c["bot"][1].Lane != "account:"+b.w.saAccount(bot).Meta.ID {
		t.Errorf("per-lane rule: %s / %s / %s lane %q, want a bucket per lane", c["user"][1].ID, c["bot"][1].ID, c["other bot"][1].ID, c["bot"][1].Lane)
	}
	if c["bot"][0].Rung != limits.RungPersonal || c["user"][0].Rung != limits.RungLane {
		t.Errorf("rungs: bot %s user %s, want personal and lane", c["bot"][0].Rung, c["user"][0].Rung)
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
