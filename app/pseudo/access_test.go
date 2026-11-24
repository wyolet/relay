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

// Ids of the rows an access body names.
const (
	plainID     = "01a20b40-0000-7000-8000-0000000000a1"
	labelledAID = "01a20b40-0000-7000-8000-0000000000a2"
	labelledBID = "01a20b40-0000-7000-8000-0000000000a3"
	teamAID     = "01a20b40-0000-7000-8000-0000000000a4"
	teamA2ID    = "01a20b40-0000-7000-8000-0000000000a5"
	teamBID     = "01a20b40-0000-7000-8000-0000000000a6"
	aliceBID    = "01a20b40-0000-7000-8000-0000000000a7"
	targetID    = "01a20b40-0000-7000-8000-0000000000a8"
)

func newAccessWorld() *accessWorld {
	acc := account.ForProject("proj", "search", "Search")
	acc.Meta.ID = "proj-acct"
	labelled := func(id, name, host string, members ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{ID: id, Name: name, Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}, ResourceVersion: "5"}, Spec: pool.Spec{HostID: host, AccountIDs: members}}
	}
	w := &accessWorld{
		acc:       acc,
		plain:     &pool.Pool{Meta: meta.Metadata{ID: plainID, Name: "plain", Owner: proj}, Spec: pool.Spec{HostID: "host-a"}},
		labelledA: labelled(labelledAID, "labelled-a", "host-a", teamAID),
		labelledB: labelled(labelledBID, "labelled-b", "host-b", teamBID),
		teamA:     &account.Account{Meta: meta.Metadata{ID: teamAID, Name: "team-a", Owner: teamOwner}, Spec: account.Spec{HostID: "host-a"}},
		teamA2:    &account.Account{Meta: meta.Metadata{ID: teamA2ID, Name: "team-a2", Owner: teamOwner}, Spec: account.Spec{HostID: "host-a"}},
		teamB:     &account.Account{Meta: meta.Metadata{ID: teamBID, Name: "team-b", Owner: teamOwner}, Spec: account.Spec{HostID: "host-b"}},
		aliceB:    &account.Account{Meta: meta.Metadata{ID: aliceBID, Name: "alice-b", Owner: alice}, Spec: account.Spec{HostID: "host-b"}},
	}
	w.own = &policy.Policy{Meta: meta.Metadata{ID: "own", Name: "own", Owner: proj, ResourceVersion: "2"}, Spec: policy.Spec{
		Models: []string{"acme"}, PoolIDs: []string{plainID, labelledAID, labelledBID},
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
		Accounts:  map[string]*account.Account{teamAID: w.teamA, teamA2ID: w.teamA2, teamBID: w.teamB, aliceBID: w.aliceB},
		HostNames: map[string]string{"host-a": "alpha", "host-b": "beta"},
		Reaches:   func(a *account.Account) bool { return a.Meta.Owner == teamOwner },
		OwnerName: "search",
		Labelled:  []*pool.Pool{w.labelledA, w.labelledB},
		LinkedBy:  map[string][]string{plainID: {"own"}, labelledAID: {"own"}, labelledBID: {"own"}},

		LabelledDeletable: map[string]bool{labelledAID: true, labelledBID: true},
	}
}

func accessBody(a *AgentAccess) Access {
	return Access{Spec: AccessSpec{Target: Target{Kind: "project", ID: targetID}, Access: a}}
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
	newA := &pool.Pool{Meta: meta.Metadata{ID: "id-1", Name: "search-at-alpha", DisplayName: "Search at alpha", Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}}, Spec: pool.Spec{HostID: "host-a", AccountIDs: []string{teamAID}}}
	for _, tc := range []struct {
		name   string
		access AgentAccess
		want   []Row
	}{
		{"reordered: the labelled pools keep their members, the unlabelled one moves last",
			AgentAccess{PoolIDs: []string{labelledBID, labelledAID, plainID}, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links(labelledBID, labelledAID, plainID))}},
		{"a host's accounts changed and one added that is shared first; the other host's pool and the unlabelled one dropped",
			AgentAccess{AccountIDs: []string{teamA2ID, aliceBID, teamAID}, Models: []string{"acme/m1"}},
			[]Row{
				{Kind: "account", Pointer: "/spec/access/accountIds/1", Meta: &shareB.Meta, Value: shareB},
				poolUpdate(w.labelledA, teamA2ID, teamAID), poolUpdate(w.labelledB, "id-1"),
				policyUpdate(w.own, links(labelledAID, labelledBID)),
			}},
		{"unchanged members are not written",
			AgentAccess{PoolIDs: []string{plainID}, AccountIDs: []string{teamBID}, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links(plainID, labelledBID)), poolDelete(w.labelledA)}},
		{"every credential: no pools, the labelled ones deleted",
			AgentAccess{EveryCredential: true, Models: []string{"acme/m1"}},
			[]Row{policyUpdate(w.own, links()), poolDelete(w.labelledA), poolDelete(w.labelledB)}},
		{"a labelled pool listed in poolIds is linked as it is, not taken for its host",
			AgentAccess{PoolIDs: []string{labelledAID}, AccountIDs: []string{teamAID}, Models: []string{"acme/m1"}},
			[]Row{
				{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &newA.Meta, Value: newA},
				policyUpdate(w.own, links(labelledAID, "id-1")), poolDelete(w.labelledB),
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

// A pool link the caller may not see is kept at its place in the order, in place and in a copy, whatever access names; access cannot name it.
func TestExpandAccess_KeepsHiddenPools(t *testing.T) {
	const hiddenID = "01a20b40-0000-7000-8000-0000000000a9"
	for _, c := range []PolicyCase{CaseOwn, CaseCopy} {
		for _, tc := range []struct {
			name   string
			access AgentAccess
			want   []string
		}{
			{"one pool named", AgentAccess{PoolIDs: []string{plainID}}, []string{plainID, hiddenID}},
			{"pools reordered", AgentAccess{PoolIDs: []string{labelledBID, plainID, labelledAID}}, []string{labelledBID, hiddenID, plainID, labelledAID}},
			{"every credential", AgentAccess{EveryCredential: true}, []string{hiddenID}},
		} {
			t.Run(string(c)+"/"+tc.name, func(t *testing.T) {
				w := newAccessWorld()
				w.own.Spec.PoolIDs = []string{plainID, hiddenID, labelledAID, labelledBID}
				in := w.reads()
				in.HiddenPools = map[string]bool{hiddenID: true}
				in.Policy.Alone = c == CaseOwn
				got, err := ExpandAccess(accessBody(&tc.access), in)
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.want, got.Policy.Spec.PoolIDs); diff != "" || got.Case != c || got.HiddenPoolsKept != 1 {
					t.Errorf("case %s, kept %d, pools (-want +got):\n%s", got.Case, got.HiddenPoolsKept, diff)
				}
			})
		}
	}
}

// A labelled pool another policy links too is never updated or deleted: the host gets a new pool.
func TestExpandAccess_LabelledPoolLinkedElsewhere(t *testing.T) {
	w := newAccessWorld()
	in := w.reads()
	in.LinkedBy[labelledAID] = []string{"own", "someone-elses"}
	got, err := ExpandAccess(accessBody(&AgentAccess{AccountIDs: []string{teamA2ID}}), in)
	if err != nil {
		t.Fatal(err)
	}
	made := &pool.Pool{Meta: meta.Metadata{ID: "id-1", Name: "search-at-alpha", DisplayName: "Search at alpha", Owner: proj, Labels: map[string]string{AccessLabel: "proj-acct"}}, Spec: pool.Spec{HostID: "host-a", AccountIDs: []string{teamA2ID}}}
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
	got, err := ExpandAccess(accessBody(&AgentAccess{AccountIDs: []string{teamAID}, Models: []string{"acme/m1"}}), in)
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
	want := []string{" pool id-1", " policy id-2", " policy-binding "}
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
		name   string
		a      *AgentAccess
		want   *BodyError
		target *Target
	}{
		{"a target by slug", &AgentAccess{EveryCredential: true}, &BodyError{Pointer: "/spec/target/id", Message: `team "research" is not an id; name the team by its metadata.id`}, &Target{Kind: "team", ID: "research"}},
		{"a target of another kind", &AgentAccess{EveryCredential: true}, &BodyError{Pointer: "/spec/target/kind", Message: "spec.target.kind must be team, project or service-account"}, &Target{Kind: "user", ID: targetID}},
		{"no access", nil, &BodyError{Pointer: "/spec/access", Message: "spec.access is required unless dryRun=true"}, nil},
		{"nothing to use", &AgentAccess{}, &BodyError{Pointer: "/spec/access", Message: "accountIds or poolIds is required unless everyCredential is true"}, nil},
		{"a pool twice", &AgentAccess{PoolIDs: []string{plainID, plainID}}, &BodyError{Pointer: "/spec/access/poolIds/1", Message: `pool "` + plainID + `" is listed twice`}, nil},
		{"a pool by slug", &AgentAccess{PoolIDs: []string{"plain"}}, &BodyError{Pointer: "/spec/access/poolIds/0", Message: `pool "plain" is not an id; name the pool by its metadata.id`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := accessBody(tc.a)
			if tc.target != nil {
				body.Spec.Target = *tc.target
			}
			_, err := ExpandAccess(body, w.reads())
			if diff := cmp.Diff(tc.want, err); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}
