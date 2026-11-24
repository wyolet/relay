package pseudo

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// accessWorld is the project account of proj under a policy of its own linking an unlabelled pool, the labelled pool at host a and the one at host b; accounts at hosts a and b the project reaches, and alice's at host b it does not.
type accessWorld struct {
	acc                          *account.Account
	own                          *policy.Policy
	plain, labelledA, labelledB  *pool.Pool
	teamA, teamB, aliceB, teamA2 *account.Account
}

func newAccessWorld() *accessWorld {
	acc := account.ForProject("proj", "search", "Search")
	acc.Meta.ID = "proj-acct"
	labelled := func(id, host string, members ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{ID: id, Name: id, Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}, ResourceVersion: "5"}, Spec: pool.Spec{HostID: host, AccountIDs: members}}
	}
	w := &accessWorld{
		acc:       acc,
		plain:     &pool.Pool{Meta: meta.Metadata{ID: "plain", Name: "plain", Owner: proj}, Spec: pool.Spec{HostID: "host-a"}},
		labelledA: labelled("labelled-a", "host-a", "team-a"),
		labelledB: labelled("labelled-b", "host-b", "team-b"),
		teamA:     &account.Account{Meta: meta.Metadata{ID: "team-a", Name: "team-a", Owner: teamOwner}, Spec: account.Spec{HostID: "host-a"}},
		teamA2:    &account.Account{Meta: meta.Metadata{ID: "team-a2", Name: "team-a2", Owner: teamOwner}, Spec: account.Spec{HostID: "host-a"}},
		teamB:     &account.Account{Meta: meta.Metadata{ID: "team-b", Name: "team-b", Owner: teamOwner}, Spec: account.Spec{HostID: "host-b"}},
		aliceB:    &account.Account{Meta: meta.Metadata{ID: "alice-b", Name: "alice-b", Owner: alice}, Spec: account.Spec{HostID: "host-b"}},
	}
	w.own = &policy.Policy{Meta: meta.Metadata{ID: "own", Name: "own", Owner: proj, ResourceVersion: "2"}, Spec: policy.Spec{
		Models: []string{"acme"}, PoolIDs: []string{"plain", "labelled-a", "labelled-b"},
		RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}}, Budgets: []policy.BudgetLink{{BudgetID: "bu"}},
	}}
	return w
}

// reads is the world with the account bound alone to its own policy, the caller allowed to update it.
func (w *accessWorld) reads() AccessReads {
	in := stepReads(w.acc)
	in.TargetLabel = "Search"
	in.Policy, in.Held, in.Alone, in.MayUpdate, in.HeldChangeable = w.own, heldBy("own", w.acc.Meta.ID), true, true, true
	return AccessReads{
		Policy:    in,
		Accounts:  map[string]*account.Account{"team-a": w.teamA, "team-a2": w.teamA2, "team-b": w.teamB, "alice-b": w.aliceB},
		HostNames: map[string]string{"host-a": "alpha", "host-b": "beta"},
		Reaches:   func(a *account.Account) bool { return a.Meta.Owner == teamOwner },
		OwnerName: "search",
		Labelled:  []*pool.Pool{w.labelledA, w.labelledB},
		LinkedBy:  map[string][]string{"plain": {"own"}, "labelled-a": {"own"}, "labelled-b": {"own"}},
	}
}

func accessBody(a *AgentAccess) Access {
	return Access{Spec: AccessSpec{Target: Target{Kind: "project", ID: "proj"}, Access: a}}
}

func policyUpdate(p *policy.Policy, spec policy.Spec) Row {
	next := *p
	next.Spec = spec
	return Row{Kind: "policy", Pointer: "/spec/access", Meta: &next.Meta, Value: &next, Op: OpUpdate}
}

func poolUpdate(p *pool.Pool, members ...string) Row {
	next := *p
	next.Spec.AccountIDs = members
	return Row{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &next.Meta, Value: &next, Op: OpUpdate}
}

func poolDelete(p *pool.Pool) Row {
	return Row{Kind: "pool", Pointer: "/spec/access", Meta: &p.Meta, Value: p, Op: OpDelete}
}

// In place, the access is stated whole: a labelled pool is updated in place for its host or deleted once nothing links it, the unlabelled pool is only linked or unlinked, and the rate limit and budget stay.
func TestExpandAccess_Own(t *testing.T) {
	w := newAccessWorld()
	links := func(pools ...string) policy.Spec {
		s := policy.Spec{PoolIDs: pools, Models: []string{"acme/m1"}, RateLimits: w.own.Spec.RateLimits, Budgets: w.own.Spec.Budgets}
		return s
	}
	shareB := account.Share(w.aliceB, account.Receiver{Kind: account.ReceiverProject, ID: "proj"}, proj, "search")
	shareB.Meta.ID, shareB.Meta.Name = "id-1", "alice-b-for-search"
	newA := &pool.Pool{Meta: meta.Metadata{ID: "id-1", Name: "search-at-alpha", DisplayName: "Search at alpha", Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}}, Spec: pool.Spec{HostID: "host-a", AccountIDs: []string{"team-a"}}}
	for _, tc := range []struct {
		name   string
		access AgentAccess
		want   []Row
	}{
		{"reordered: the labelled pools keep their members, the unlabelled one moves last",
			AgentAccess{PoolIDs: []string{"labelled-b", "labelled-a", "plain"}, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links("labelled-b", "labelled-a", "plain"))}},
		{"a host's accounts changed and one added that is shared first; the other host's pool and the unlabelled one dropped",
			AgentAccess{AccountIDs: []string{"team-a2", "alice-b", "team-a"}, Models: []string{"acme/m1"}},
			[]Row{
				{Kind: "account", Pointer: "/spec/access/accountIds/1", Meta: &shareB.Meta, Value: shareB},
				poolUpdate(w.labelledA, "team-a2", "team-a"), poolUpdate(w.labelledB, "id-1"),
				policyUpdate(w.own, links("labelled-a", "labelled-b")),
			}},
		{"unchanged members are not written",
			AgentAccess{PoolIDs: []string{"plain"}, AccountIDs: []string{"team-b"}, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links("plain", "labelled-b")), poolDelete(w.labelledA)}},
		{"every credential: no pools, the labelled ones deleted",
			AgentAccess{EveryCredential: true, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links()), poolDelete(w.labelledA), poolDelete(w.labelledB)}},
		{"a labelled pool listed in poolIds is linked as it is, not taken for its host",
			AgentAccess{PoolIDs: []string{"labelled-a"}, AccountIDs: []string{"team-a"}, Models: []string{"acme/m1"}},
			[]Row{
				{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &newA.Meta, Value: newA},
				policyUpdate(w.own, links("labelled-a", "id-1")), poolDelete(w.labelledB),
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandAccess(accessBody(&tc.access), w.reads())
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got.Rows); diff != "" || got.Case != CaseOwn {
				t.Errorf("case %s, rows (-want +got):\n%s", got.Case, diff)
			}
		})
	}
}

// A labelled pool another policy links too is never updated or deleted: the host gets a new pool.
func TestExpandAccess_LabelledPoolLinkedElsewhere(t *testing.T) {
	w := newAccessWorld()
	in := w.reads()
	in.LinkedBy["labelled-a"] = []string{"own", "someone-elses"}
	got, err := ExpandAccess(accessBody(&AgentAccess{AccountIDs: []string{"team-a2"}}), in)
	if err != nil {
		t.Fatal(err)
	}
	made := &pool.Pool{Meta: meta.Metadata{ID: "id-1", Name: "search-at-alpha", DisplayName: "Search at alpha", Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}}, Spec: pool.Spec{HostID: "host-a", AccountIDs: []string{"team-a2"}}}
	want := []Row{
		{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &made.Meta, Value: made},
		policyUpdate(w.own, policy.Spec{PoolIDs: []string{"id-1"}, RateLimits: w.own.Spec.RateLimits, Budgets: w.own.Spec.Budgets}),
		poolDelete(w.labelledB),
	}
	if diff := cmp.Diff(want, got.Rows); diff != "" {
		t.Errorf("rows (-want +got):\n%s", diff)
	}
}

// In a copy every pool the source links stays as it is: the copy gets new labelled pools, and the source's are not deleted.
func TestExpandAccess_Copy(t *testing.T) {
	w := newAccessWorld()
	in := w.reads()
	in.Policy.Alone = false
	got, err := ExpandAccess(accessBody(&AgentAccess{AccountIDs: []string{"team-a"}, Models: []string{"acme/m1"}}), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Case != CaseCopy {
		t.Fatalf("case %s", got.Case)
	}
	var kinds []string
	for _, r := range got.Rows {
		kinds = append(kinds, string(r.Op)+" "+r.Kind+" "+r.Meta.ID)
	}
	want := []string{" pool id-1", " policy id-2", "delete policy-binding held", " policy-binding id-3"}
	if diff := cmp.Diff(want, kinds); diff != "" {
		t.Errorf("rows (-want +got):\n%s", diff)
	}
	if p := got.Policy; p.Meta.Owner != proj || !cmp.Equal(p.Spec.PoolIDs, []string{"id-1"}) || len(p.Spec.Budgets) != 1 || len(p.Spec.RateLimits) != 1 {
		t.Errorf("copy = %+v", p)
	}
}

// With no binding and nothing to narrow, nothing is written; with models, a policy of them linking no pool is made and bound.
func TestExpandAccess_None(t *testing.T) {
	w := newAccessWorld()
	in := w.reads()
	in.Policy.Held, in.Policy.Policy = nil, nil
	got, err := ExpandAccess(accessBody(&AgentAccess{EveryCredential: true}), in)
	if err != nil || !got.Unchanged || got.Case != CaseNone || len(got.Rows) != 0 {
		t.Fatalf("every credential, no models: %+v %v", got, err)
	}
	got, err = ExpandAccess(accessBody(&AgentAccess{EveryCredential: true, Models: []string{"acme"}}), in)
	if err != nil || got.Unchanged || len(got.Rows) != 2 || got.Policy.Spec.PoolIDs != nil || !cmp.Equal(got.Policy.Spec.Models, []string{"acme"}) {
		t.Fatalf("every credential with models: %+v %v", got, err)
	}
}

func TestExpandAccess_RefusesBody(t *testing.T) {
	w := newAccessWorld()
	for _, tc := range []struct {
		name string
		a    *AgentAccess
		want *BodyError
	}{
		{"no access", nil, &BodyError{Pointer: "/spec/access", Message: "spec.access is required unless dryRun=true"}},
		{"nothing to use", &AgentAccess{}, &BodyError{Pointer: "/spec/access", Message: "accountIds or poolIds is required unless everyCredential is true"}},
		{"a pool twice", &AgentAccess{PoolIDs: []string{"plain", "plain"}}, &BodyError{Pointer: "/spec/access/poolIds/1", Message: `pool "plain" is listed twice`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExpandAccess(accessBody(tc.a), w.reads())
			if diff := cmp.Diff(tc.want, err); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}
