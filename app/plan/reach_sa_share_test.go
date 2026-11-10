package plan

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
)

// saShare is a project policy bound to its service account, whose only pool lists an account shared to that service account.
type saShare struct {
	b      *basic
	share  *account.Account
	pool   *pool.Pool
	pol    *policy.Policy
	saAcct *account.Account
	h      *harness
}

func newSAShare(t *testing.T, bound bool) *saShare {
	b := newBasic()
	projOwner := meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}
	s := &saShare{b: b, saAcct: b.w.saAccount(b.sa)}
	s.share = b.w.share(b.acctA, account.Receiver{Kind: account.ReceiverServiceAccount, ID: b.sa.Meta.ID}, projOwner)
	s.pool = b.w.pool(b.hostA)
	s.pool.Meta.Owner = projOwner
	s.pool.Spec.AccountIDs = []string{s.share.Meta.ID}
	s.pol = b.w.policy("web-ci", policy.Spec{PoolIDs: []string{s.pool.Meta.ID}})
	s.pol.Meta.Owner = projOwner
	if bound {
		b.w.bindPolicy(s.pol, s.saAcct)
	}
	s.h = b.w.start(t)
	return s
}

func (s *saShare) draft() *policy.Policy {
	return &policy.Policy{Meta: meta.Metadata{Owner: s.pol.Meta.Owner}, Spec: policy.Spec{PoolIDs: []string{s.pool.Meta.ID}}}
}

// Read in the project's own account, the share serves nothing; the answer says so instead of coming back empty.
func TestReach_ShareHeldForServiceAccount_DroppedInProjectAccount(t *testing.T) {
	s := newSAShare(t, false)
	projAcct := s.b.w.projectAccount(s.b.proj).Meta.ID
	r := ReachOf(s.h.e.View(), projAcct, &Override{AccountID: projAcct, Policy: s.draft()})
	if len(r.Models) != 0 {
		t.Fatalf("models %v, want none in the project's account", modelNames(r.Models))
	}
	if len(r.Dropped) != 1 {
		t.Fatalf("dropped = %+v, want the share", r.Dropped)
	}
	d := r.Dropped[0]
	if d.Code != DroppedHeldForAnotherLane || d.Account.Meta.ID != s.share.Meta.ID || d.Pool.Meta.ID != s.pool.Meta.ID ||
		!strings.Contains(d.Reason, "service account "+s.b.sa.Meta.Name) || !strings.HasSuffix(d.Reason, "pass account="+s.saAcct.Meta.ID) {
		t.Errorf("dropped = %+v", d)
	}
}

// Read as the service account's own policy, the draft reaches what the share serves.
func TestReach_ShareHeldForServiceAccount_DraftInItsAccount(t *testing.T) {
	s := newSAShare(t, false)
	r := ReachOf(s.h.e.View(), s.saAcct.Meta.ID, &Override{AccountID: s.saAcct.Meta.ID, Policy: s.draft()})
	if len(r.Models) != 1 || r.Models[0].Model.Meta.ID != s.b.m1.Meta.ID || len(r.Dropped) != 0 {
		t.Errorf("models %v dropped %+v, want m1 and nothing dropped", modelNames(r.Models), r.Dropped)
	}
}

// A bound saved policy is read in its bound accounts: the service account reaches m1, and says so.
func TestPolicyReach_BoundToServiceAccount(t *testing.T) {
	s := newSAShare(t, true)
	r := PolicyReach(s.h.e.View(), get[policy.Policy](s.h, s.pol.Meta.ID))
	if r == nil || len(r.Models) != 1 {
		t.Fatalf("reach = %+v, want m1", r)
	}
	if a := r.Models[0].Accounts; len(a) != 1 || a[0].Meta.ID != s.saAcct.Meta.ID {
		t.Errorf("accounts = %+v, want the service account", a)
	}
}

// Merged over bound accounts, a model only some reach is reached, with those accounts; one none reaches is excluded with the accounts it holds for.
func TestPolicyReach_MergesLadders(t *testing.T) {
	b, m2 := reachWorld()
	pa := b.w.pool(b.hostA, b.a1)
	b.w.bindPolicy(b.w.policy("platform-default", policy.Spec{Models: []string{"acme/m1"}}), b.w.teamAccount(b.team))
	proj2 := b.w.project(b.w.team())
	pol := b.w.policy("shared-by-two", policy.Spec{PoolIDs: []string{pa.Meta.ID}})
	inTeam := b.w.member(b.w.user(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	elsewhere := b.w.member(b.w.user(), meta.Owner{Kind: meta.OwnerProject, ID: proj2.Meta.ID})
	b.w.bindPolicy(pol, inTeam, elsewhere)
	teamOnly := b.w.policy("team-only", policy.Spec{PoolIDs: []string{pa.Meta.ID}})
	inTeam2 := b.w.member(b.w.user(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	b.w.bindPolicy(teamOnly, inTeam2)
	h := b.w.start(t)
	if r := PolicyReach(h.e.View(), get[policy.Policy](h, teamOnly.Meta.ID)); len(r.Excluded) != 1 || r.Excluded[0].Model.Meta.ID != m2.Meta.ID || len(r.Excluded[0].Accounts) != 1 || r.Excluded[0].Refused.Code != CodeModelNotAllowed {
		t.Errorf("team-only excluded %+v, want m2 refused for its one account", r.Excluded)
	}
	r := PolicyReach(h.e.View(), get[policy.Policy](h, pol.Meta.ID))
	for _, m := range r.Models {
		want := 2
		if m.Model.Meta.ID == m2.Meta.ID {
			want = 1
		}
		if len(m.Accounts) != want {
			t.Errorf("%s reached by %d accounts, want %d", m.Model.Meta.Name, len(m.Accounts), want)
		}
	}
	if len(r.Models) != 2 || len(r.Excluded) != 0 {
		t.Errorf("models %v excluded %v, want m1 and m2 reached, m2 by one account", modelNames(r.Models), modelNames(r.Excluded))
	}
}
