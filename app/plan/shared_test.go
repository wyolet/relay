package plan

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

// sharing is one host serving m1 and m2, and alice's provider account there with one key: the real account the tests share.
type sharing struct {
	w      *world
	host   *host.Host
	m1, m2 *model.Model
	alice  *user.User
	real   *account.Account
	key    *hostkey.HostKey
	team   *team.Team
}

func newSharing() *sharing {
	w := &world{}
	s := &sharing{w: w}
	prov := w.provider("acme")
	s.host = w.host("host-a")
	s.m1, s.m2 = w.model(prov, "m1"), w.model(prov, "m2")
	for _, m := range []*model.Model{s.m1, s.m2} {
		w.bind(m, s.host)
		w.price(m, s.host)
	}
	s.alice = w.user()
	s.real = w.ownedAccount(s.host, userOwner(s.alice))
	s.key = w.cred(s.host, s.real, userOwner(s.alice))
	s.team = w.team()
	return s
}

func userOwner(u *user.User) meta.Owner { return meta.Owner{Kind: meta.OwnerUser, ID: u.ID} }

func (w *world) ownedAccount(h *host.Host, owner meta.Owner) *account.Account {
	a := &account.Account{Meta: md("acct-"+seq(), owner), Spec: account.Spec{HostID: h.Meta.ID, Kind: account.KindProvider}}
	w.accounts = append(w.accounts, a)
	return a
}

// share adds src shared with r, owned by owner.
func (w *world) share(src *account.Account, r account.Receiver, owner meta.Owner) *account.Account {
	a := account.Share(src, r, owner, "r-"+seq())
	a.Meta.ID = meta.NewID()
	w.accounts = append(w.accounts, a)
	return a
}

func (s *sharing) shareWithUser(u *user.User) *account.Account {
	return s.w.share(s.real, account.Receiver{Kind: account.ReceiverUser, ID: u.ID}, userOwner(u))
}

func counterOn(cs []limits.Counter, subject string) *limits.Counter {
	for i := range cs {
		if cs[i].SubjectID == subject {
			return &cs[i]
		}
	}
	return nil
}

// A user's key in an account shared with them runs on the real account's key: the slot names the real account (whose bill it is) and the share it went through, and counts the real account's tier in the same bucket the owner's own requests do.
func TestShare_LaneDrawsOnTheRealAccount(t *testing.T) {
	s := newSharing()
	bob := s.w.user()
	shared := s.shareWithUser(bob)
	tier := s.w.rateLimit(rule(ratelimit.MeterRequests, 1000, time.Minute))
	s.w.bindPolicy(s.w.policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: tier.Meta.ID}}}), s.real)
	fair := s.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	s.w.bindPolicy(s.w.policy("fair", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: fair.Meta.ID}}}), shared)
	bobKey, aliceKey := s.w.keyIn(bob, shared), s.w.userKey(s.alice)
	h := s.w.start(t)

	r := h.route(bobKey, "m1")
	if r.Refused != nil || !slices.Equal(slotIDs(r)[0], ids(s.key)) {
		t.Fatalf("route %v refused %v, want alice's key", slotIDs(r), r.Refused)
	}
	slot := r.Hops[0].Slots[0]
	if slot.Account.Account.Meta.ID != s.real.Meta.ID || slot.Via.Account.Meta.ID != shared.Meta.ID {
		t.Fatalf("slot account %s via %v, want the real account via the share", slot.Account.Account.Meta.ID, slot.Via.Account)
	}
	up := r.Hops[0].UpstreamCounters(0)
	realSubject, shareSubject := accountSubject(s.real.Meta.ID), accountSubject(shared.Meta.ID)
	tierCt, fairCt := counterOn(up, realSubject), counterOn(up, shareSubject)
	if tierCt == nil || fairCt == nil {
		t.Fatalf("upstream counters %+v, want the real account's tier and the share's own", up)
	}
	if fairCt.Group != realSubject {
		t.Errorf("share counter grouped under %s, want the real account's %s so one store call takes both", fairCt.Group, realSubject)
	}
	own := counterOn(h.route(aliceKey, "m1").Hops[0].UpstreamCounters(0), realSubject)
	if own == nil || own.ID != tierCt.ID {
		t.Errorf("owner's tier counter %+v, want the bucket the share counts in (%s)", own, tierCt.ID)
	}
	if counterOn(h.route(aliceKey, "m1").Hops[0].UpstreamCounters(0), shareSubject) != nil {
		t.Error("the owner's own requests count on the share's limit")
	}
	if !slot.CountsOnAccount(tierCt) || slot.CountsOnAccount(fairCt) {
		t.Error("CountsOnAccount mistakes the share's counter for the real account's")
	}
}

// A share's own policy only narrows: a model it leaves out is refused, naming the share's rung, while the owner still reaches it.
func TestShare_PolicyNarrows(t *testing.T) {
	s := newSharing()
	bob := s.w.user()
	shared := s.shareWithUser(bob)
	s.w.bindPolicy(s.w.policy("m2-only", policy.Spec{Models: []string{"acme/m2"}}), shared)
	bobKey, aliceKey := s.w.keyIn(bob, shared), s.w.userKey(s.alice)
	h := s.w.start(t)

	r := h.route(bobKey, "m1")
	if r.Refused == nil || r.Refused.Code != CodeModelNotAllowed || r.Refused.SubjectID != accountSubject(shared.Meta.ID) {
		t.Fatalf("m1 through the share: %+v, want model_not_allowed by the share's rung", r.Refused)
	}
	if r := h.route(bobKey, "m2"); r.Refused != nil {
		t.Errorf("m2 through the share refused: %v", r.Refused)
	}
	if r := h.route(aliceKey, "m1"); r.Refused != nil {
		t.Errorf("the owner's m1 refused: %v", r.Refused)
	}
}

// A share whose real account is gone serves nothing and says why.
func TestShare_RealAccountGone(t *testing.T) {
	s := newSharing()
	bob := s.w.user()
	shared := s.shareWithUser(bob)
	bobKey := s.w.keyIn(bob, shared)
	h := s.w.start(t)
	if r := h.route(bobKey, "m1"); r.Refused != nil {
		t.Fatalf("before: %v", r.Refused)
	}

	del(h, KindAccount, s.real.Meta.ID)
	if r := h.route(bobKey, "m1"); r.Refused == nil || r.Refused.Code != CodeNoCredentials {
		t.Fatalf("after the real account went: %+v, want no_credentials", r.Refused)
	}
	if is := h.issues(KindAccount, shared.Meta.ID); !hasIssue(is, IssueSharedAccountMissing) {
		t.Errorf("issues on the share = %+v, want %s", is, IssueSharedAccountMissing)
	}
}

// Keys added to the real account and rotations of its keys reach every share on the next plan, with the breaker of the new value.
func TestShare_FollowsTheRealAccountsKeys(t *testing.T) {
	s := newSharing()
	bob := s.w.user()
	shared := s.shareWithUser(bob)
	bobKey := s.w.keyIn(bob, shared)
	h := s.w.start(t)

	rotated := clone(s.key)
	rotate(rotated, "sk-rotated")
	apply(h, rotated)
	slot := h.route(bobKey, "m1").Hops[0].Slots[0]
	if slot.Credential.KeyHash != rotated.KeyHash || slot.Breaker != get[KeySlot](h, s.key.Meta.ID).Breaker {
		t.Fatalf("after rotation the share uses hash %s, want %s and the real slot's breaker", slot.Credential.KeyHash, rotated.KeyHash)
	}

	added := &hostkey.HostKey{Meta: md("cred-added", userOwner(s.alice)), Spec: hostkey.Spec{HostID: s.host.Meta.ID, AccountID: s.real.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}}
	rotate(added, "sk-added")
	apply(h, added)
	if got := slotIDs(h.route(bobKey, "m1")); len(got) != 2 || !slices.Contains(got[1], added.Meta.ID) {
		t.Fatalf("route %v, want the added key reached through the share", got)
	}
}

// An account shared with a team is reached by its members by ownership, as the team's own keys are.
func TestShare_TeamMembersReachIt(t *testing.T) {
	s := newSharing()
	teamOwner := meta.Owner{Kind: meta.OwnerTeam, ID: s.team.Meta.ID}
	shared := s.w.share(s.real, account.Receiver{Kind: account.ReceiverTeam, ID: s.team.Meta.ID}, teamOwner)
	carol := s.w.user()
	key := s.w.keyIn(carol, s.w.member(carol, teamOwner))
	outsider := s.w.user()
	outsiderKey := s.w.userKey(outsider)
	h := s.w.start(t)

	r := h.route(key, "m1")
	if r.Refused != nil || r.Hops[0].Slots[0].Via.Account.Meta.ID != shared.Meta.ID {
		t.Fatalf("team member's route %v refused %v, want the shared key", slotIDs(r), r.Refused)
	}
	if r := h.route(outsiderKey, "m1"); r.Refused == nil {
		t.Errorf("a user outside the team reaches %v", slotIDs(r))
	}
}

// A service account's share is owned by its project but held by it: only its lane draws on it, not the project's other lanes.
func TestShare_ServiceAccountOnly(t *testing.T) {
	s := newSharing()
	proj := s.w.project(s.team)
	mine, other := s.w.sa(proj), s.w.sa(proj)
	s.w.share(s.real, account.Receiver{Kind: account.ReceiverServiceAccount, ID: mine.Meta.ID}, meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID})
	mineKey, otherKey := s.w.saKey(mine), s.w.saKey(other)
	h := s.w.start(t)

	if r := h.route(mineKey, "m1"); r.Refused != nil || !slices.Equal(slotIDs(r)[0], ids(s.key)) {
		t.Fatalf("the holder's route %v refused %v", slotIDs(r), r.Refused)
	}
	if r := h.route(otherKey, "m1"); r.Refused == nil {
		t.Errorf("another service account of the project reaches %v", slotIDs(r))
	}
}

// A pool tries its accounts first, in their order and each account's keys in creation order, then its credentials; a key reached twice keeps its first place.
func TestPool_AccountsThenCredentials(t *testing.T) {
	s := newSharing()
	second := s.w.cred(s.host, s.real, userOwner(s.alice))
	other := s.w.ownedAccount(s.host, system)
	otherKey := s.w.cred(s.host, other, system)
	loose := s.w.cred(s.host, nil, system)
	p := s.w.pool(s.host, loose, s.key)
	p.Spec.AccountIDs = []string{other.Meta.ID, s.real.Meta.ID}
	h := s.w.start(t)

	pp := get[PoolPlan](h, p.Meta.ID)
	var got []string
	for _, sl := range pp.Slots {
		got = append(got, sl.Credential.Meta.ID)
	}
	if want := ids(otherKey, s.key, second, loose); !slices.Equal(got, want) {
		t.Fatalf("pool order %v, want %v", got, want)
	}
}

// A pool lists a share of its own scope and serves it to the scope's lanes through a policy; a share of another scope that a row written around the save check lists is left out, with an issue.
func TestPool_DrawsOnAShare(t *testing.T) {
	s := newSharing()
	teamOwner := meta.Owner{Kind: meta.OwnerTeam, ID: s.team.Meta.ID}
	shared := s.w.share(s.real, account.Receiver{Kind: account.ReceiverTeam, ID: s.team.Meta.ID}, teamOwner)
	proj, sibling := s.w.project(s.team), s.w.project(s.team)
	elsewhere := s.w.share(s.real, account.Receiver{Kind: account.ReceiverProject, ID: sibling.Meta.ID}, meta.Owner{Kind: meta.OwnerProject, ID: sibling.Meta.ID})
	p := s.w.pool(s.host)
	p.Meta.Owner = teamOwner
	p.Spec.AccountIDs = []string{elsewhere.Meta.ID, shared.Meta.ID}
	pol := s.w.policy("team-pool", policy.Spec{PoolIDs: []string{p.Meta.ID}})
	pol.Meta.Owner = teamOwner
	sa := s.w.sa(proj)
	b := s.w.bindPolicy(pol, s.w.saAccount(sa))
	key := s.w.saKey(sa)
	h := s.w.start(t)

	r := h.route(key, "m1")
	if r.Refused != nil || len(r.Hops) != 1 || len(r.Hops[0].Slots) != 1 || r.Hops[0].Slots[0].Via.Account.Meta.ID != shared.Meta.ID {
		t.Fatalf("route %v refused %v, want the team's share alone", slotIDs(r), r.Refused)
	}
	if is := h.issues(KindPolicyBinding, b.Meta.ID); !hasIssue(is, IssueLinkOutOfScope) {
		t.Errorf("binding issues %+v, want %s for the sibling project's share", is, IssueLinkOutOfScope)
	}
	if got := len(get[PoolPlan](h, p.Meta.ID).Slots); got != 2 {
		t.Errorf("pool slots = %d, want 2: each share reaches the key under its own limits", got)
	}
}
