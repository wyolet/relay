package plan

import (
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// ownedBy files a binding under owner, as the control API stores it.
func ownedBy(b *policybinding.PolicyBinding, owner meta.Owner) *policybinding.PolicyBinding {
	b.Meta.Owner = owner
	return b
}

// A binding owned by one project and naming another project's service account's account is ignored for it with an issue, however it was written. A team-owned one binds the accounts of the team's projects.
func TestBindingOutsideItsOwnersScopeNeverApplies(t *testing.T) {
	b := newBasic()
	elsewhere := b.w.project(b.team)
	pol := b.w.policy("only-m1", policy.Spec{Models: []string{"acme/m1"}})
	foreign := ownedBy(b.w.bindPolicy(pol, b.w.saAccount(b.sa)), meta.Owner{Kind: meta.OwnerProject, ID: elsewhere.Meta.ID})
	h := b.w.start(t)

	if rung := h.keyPlan(b.key).Caller.LaneRung(); rung.Policy != nil {
		t.Fatalf("service account governed by another project's binding: %+v", rung.Policy)
	}
	if !hasIssue(h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope) {
		t.Fatalf("issues = %+v, want %s", h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope)
	}

	team := clone(foreign)
	team.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	apply(h, team)
	if rung := h.keyPlan(b.key).Caller.LaneRung(); rung.Policy == nil || rung.Policy.Policy.Meta.ID != pol.Meta.ID {
		t.Fatalf("service account under its team's binding = %+v, want the bound policy", rung.Policy)
	}
	if hasIssue(h.issues(KindPolicyBinding, foreign.Meta.ID), IssueSubjectOutOfScope) {
		t.Fatal("the out-of-scope issue stayed after the binding moved to the team")
	}
}

// A binding written around the save check, whose policy links another tenant's pool and a shared pool holding another tenant's key, reaches neither for its account: the pool and the slot are left out with an issue, the account's own scope keeps the rest.
func TestLinksOutsideTheAccountsScopeAreIgnored(t *testing.T) {
	b := newBasic()
	elsewhere := b.w.project(b.w.team())
	theirs := meta.Owner{Kind: meta.OwnerProject, ID: elsewhere.Meta.ID}
	theirKey := b.w.cred(b.hostA, b.acctA, theirs)
	theirPool := b.w.pool(b.hostA, theirKey)
	theirPool.Meta.Owner = theirs
	shared := b.w.pool(b.hostA, theirKey, b.a1)
	pol := b.w.policy("borrows", policy.Spec{PoolIDs: []string{theirPool.Meta.ID, shared.Meta.ID}})
	pb := ownedBy(b.w.bindPolicy(pol, b.w.saAccount(b.sa)), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	h := b.w.start(t)

	pools := h.keyPlan(b.key).Caller.Pools
	if len(pools) != 1 || pools[0].Pool.Meta.ID != shared.Meta.ID {
		t.Fatalf("pools = %d, want only the shared pool", len(pools))
	}
	if got := ids(slotCreds(pools[0])...); len(got) != 1 || got[0] != b.a1.Meta.ID {
		t.Fatalf("shared pool slots = %v, want only the system key %s", got, b.a1.Meta.ID)
	}
	if r := h.route(b.key, "m1"); r.Refused != nil {
		t.Fatalf("route over the usable key refused: %+v", r.Refused)
	}
	is := h.issues(KindPolicyBinding, pb.Meta.ID)
	n := 0
	for _, i := range is {
		if i.Code == IssueLinkOutOfScope {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("issues = %+v, want %s for the pool and the key", is, IssueLinkOutOfScope)
	}
}

func slotCreds(p *PoolPlan) []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, 0, len(p.Slots))
	for _, s := range p.Slots {
		out = append(out, s.Credential)
	}
	return out
}
