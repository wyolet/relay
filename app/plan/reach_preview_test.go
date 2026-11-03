package plan

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/engine"
)

// reachWorld is basic plus m2, bound at host-a only.
func reachWorld() (*basic, *model.Model) {
	b := newBasic()
	m2 := b.w.model(b.prov, "m2")
	b.w.bind(m2, b.hostA)
	b.w.price(m2, b.hostA)
	return b, m2
}

func modelNames(ms []ModelReach) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Model.Meta.Name)
	}
	return out
}

func excludedBy(r *Reach, name string) *ModelReach {
	for i := range r.Excluded {
		if r.Excluded[i].Model.Meta.Name == name {
			return &r.Excluded[i]
		}
	}
	return nil
}

// A draft and the saved policy with the same owner and pools give the same answer.
func TestReach_DraftMatchesSaved(t *testing.T) {
	b, _ := reachWorld()
	pa := b.w.pool(b.hostA, b.a1)
	team := meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}
	saved := b.w.policy("saved", policy.Spec{PoolIDs: []string{pa.Meta.ID}})
	saved.Meta.Owner = team
	h := b.w.start(t)
	teamAcct := b.w.teamAccount(b.team).Meta.ID

	draft := &policy.Policy{Meta: meta.Metadata{Owner: team}, Spec: policy.Spec{PoolIDs: []string{pa.Meta.ID}}}
	got := ReachOf(h.e.View(), teamAcct, &Override{AccountID: teamAcct, Policy: draft})
	want := ReachOf(h.e.View(), teamAcct, &Override{AccountID: teamAcct, Policy: get[policy.Policy](h, saved.Meta.ID)})
	if got == nil || want == nil {
		t.Fatalf("draft %v saved %v", got, want)
	}
	if !slices.Equal(modelNames(got.Models), modelNames(want.Models)) || !slices.Equal(modelNames(got.Excluded), modelNames(want.Excluded)) {
		t.Errorf("draft %v / %v, saved %v / %v", modelNames(got.Models), modelNames(got.Excluded), modelNames(want.Models), modelNames(want.Excluded))
	}
	if !slices.Equal(modelNames(got.Models), []string{"m1", "m2"}) {
		t.Errorf("reached %v, want m1 and m2 at host-a", modelNames(got.Models))
	}
}

// Pools keep their order: what brings a model and the hosts serving it follow the draft's pool list.
func TestReach_PoolOrderKept(t *testing.T) {
	b, _ := reachWorld()
	pa, pb := b.w.pool(b.hostA, b.a1), b.w.pool(b.hostB, b.b1)
	h := b.w.start(t)
	teamAcct := b.w.teamAccount(b.team).Meta.ID
	for _, order := range [][]string{{pb.Meta.ID, pa.Meta.ID}, {pa.Meta.ID, pb.Meta.ID}} {
		draft := &policy.Policy{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}}, Spec: policy.Spec{PoolIDs: order}}
		r := ReachOf(h.e.View(), teamAcct, &Override{AccountID: teamAcct, Policy: draft})
		m1 := r.Models[slices.IndexFunc(r.Models, func(m ModelReach) bool { return m.Model.Meta.Name == "m1" })]
		var got []string
		for _, v := range m1.Via {
			got = append(got, v.Pool.Meta.ID)
		}
		if !slices.Equal(got, order) || len(m1.Hosts) != 2 {
			t.Errorf("via pools %v hosts %d, want %v over 2 hosts", got, len(m1.Hosts), order)
		}
	}
}

// A shared account brings its real account's keys: the share is what the model is reached through.
func TestReach_SharedAccount(t *testing.T) {
	s := newSharing()
	teamOwner := meta.Owner{Kind: meta.OwnerTeam, ID: s.team.Meta.ID}
	share := s.w.share(s.real, account.Receiver{Kind: account.ReceiverTeam, ID: s.team.Meta.ID}, teamOwner)
	p := s.w.pool(s.host)
	p.Meta.Owner = teamOwner
	p.Spec.AccountIDs = []string{share.Meta.ID}
	h := s.w.start(t)
	teamAcct := s.w.teamAccount(s.team).Meta.ID
	r := ReachOf(h.e.View(), teamAcct, &Override{AccountID: teamAcct, Policy: &policy.Policy{Meta: meta.Metadata{Owner: teamOwner}, Spec: policy.Spec{PoolIDs: []string{p.Meta.ID}}}})
	if len(r.Models) != 2 {
		t.Fatalf("reached %v, excluded %v", modelNames(r.Models), modelNames(r.Excluded))
	}
	m := r.Models[0]
	if len(m.Via) != 1 || m.Via[0].Account.Meta.ID != share.Meta.ID || len(m.Upstream) != 1 || m.Upstream[0].SharedAs.Meta.ID != share.Meta.ID || m.Upstream[0].Account.Meta.ID != s.real.Meta.ID {
		t.Errorf("via %+v upstream %+v, want the share over alice's account", m.Via, m.Upstream)
	}
}

// A user-owned draft of a user whose personal accounts are off refuses as a request in their personal account does.
func TestReach_PersonalOffOwner(t *testing.T) {
	s := newSharing()
	s.w.users[0].PersonalAccount = settings.PersonalAccountsOff
	h := s.w.start(t)
	personal := s.w.personal(s.alice).Meta.ID
	r := ReachOf(h.e.View(), personal, &Override{AccountID: personal, Policy: &policy.Policy{Meta: meta.Metadata{Owner: userOwner(s.alice)}}})
	if r == nil || r.Refused == nil || r.Refused.Code != CodePersonalAccountOff || len(r.Models) > 0 {
		t.Errorf("reach = %+v, want refused with %s", r, CodePersonalAccountOff)
	}
}

// A provider account's tier that leaves a model out excludes it, naming the tier and the account.
func TestReach_TierExclusion(t *testing.T) {
	b, m2 := reachWorld()
	tier := b.w.policy("build-tier", policy.Spec{Models: []string{"acme/m1"}})
	b.w.bindPolicy(tier, b.acctA)
	pa := b.w.pool(b.hostA, b.a1)
	h := b.w.start(t)
	teamAcct := b.w.teamAccount(b.team).Meta.ID
	r := ReachOf(h.e.View(), teamAcct, &Override{AccountID: teamAcct, Policy: &policy.Policy{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: b.team.Meta.ID}}, Spec: policy.Spec{PoolIDs: []string{pa.Meta.ID}}}})
	ex := excludedBy(r, m2.Meta.Name)
	if ex == nil || ex.Refused.Rung != limits.RungAccount || ex.Refused.Code != CodeModelNotAllowed {
		t.Fatalf("m2 = %+v, want excluded by the account rung", ex)
	}
	if reason := ReasonOf(h.e.View(), ex.Refused); reason != "not allowed by policy build-tier on provider account "+b.acctA.Meta.Name {
		t.Errorf("reason = %q", reason)
	}
	if len(ex.Upstream) != 1 || ex.Upstream[0].Refused == nil || ex.Upstream[0].Policy.Meta.ID != tier.Meta.ID {
		t.Errorf("upstream = %+v, want the tier refusing", ex.Upstream)
	}
}

// A project-owned draft sits under the team's policy: a model the team leaves out is excluded naming the team rung, and the team's pools narrow reach.
func TestReach_OuterRungNarrows(t *testing.T) {
	b, m2 := reachWorld()
	pa, pb := b.w.pool(b.hostA, b.a1), b.w.pool(b.hostB, b.b1)
	teamPol := b.w.policy("platform-default", policy.Spec{Models: []string{"acme/m1"}, PoolIDs: []string{pa.Meta.ID}})
	b.w.bindPolicy(teamPol, b.w.teamAccount(b.team))
	h := b.w.start(t)
	projAcct := b.w.projectAccount(b.proj).Meta.ID
	draft := &policy.Policy{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}}, Spec: policy.Spec{PoolIDs: []string{pa.Meta.ID, pb.Meta.ID}}}
	r := ReachOf(h.e.View(), projAcct, &Override{AccountID: projAcct, Policy: draft})
	ex := excludedBy(r, m2.Meta.Name)
	if ex == nil || ex.Refused.Rung != limits.RungTeam {
		t.Fatalf("m2 = %+v, want excluded by the team rung", ex)
	}
	if reason := ReasonOf(h.e.View(), ex.Refused); reason != "not allowed by team "+b.team.Meta.Name+"'s policy platform-default" {
		t.Errorf("reason = %q", reason)
	}
	if len(r.Models) != 1 || len(r.Models[0].Hosts) != 1 || r.Models[0].Hosts[0].Meta.ID != b.hostA.Meta.ID {
		t.Errorf("reached %+v, want m1 at host-a alone: the team links pool A only", r.Models)
	}
}

// With no override, an account's reach is what its published lane routes.
func TestReach_AccountMatchesListing(t *testing.T) {
	b, _ := reachWorld()
	h := b.w.start(t)
	lane := b.w.saAccount(b.sa).Meta.ID
	r := ReachOf(h.e.View(), lane, nil)
	var listed []string
	for _, l := range h.keyPlan(b.key).Caller.Routes.Listing() {
		listed = append(listed, l.Model.Meta.Name)
	}
	if !slices.Equal(modelNames(r.Models), listed) {
		t.Errorf("reach %v, listing %v", modelNames(r.Models), listed)
	}
}

// model_ref_blocked names the rung above that keeps an entry from the bound accounts, with a count when only some are kept.
func TestModelRefBlocked(t *testing.T) {
	b, _ := reachWorld()
	pa := b.w.pool(b.hostA, b.a1)
	teamPol := b.w.policy("platform-default", policy.Spec{Models: []string{"acme/m1"}})
	b.w.bindPolicy(teamPol, b.w.teamAccount(b.team))
	team2 := b.w.team()
	proj2 := b.w.project(team2)
	pol := b.w.policy("proj", policy.Spec{Models: []string{"acme/m1", "acme/m2"}, PoolIDs: []string{pa.Meta.ID}})
	b.w.bindPolicy(pol, b.w.projectAccount(b.proj))
	h := b.w.start(t)

	i := issueAt(h.issues(KindPolicy, pol.Meta.ID), IssueModelRefBlocked, "spec.models[1]")
	if i == nil || i.Severity != engine.SeverityWarning || i.Target.ID != teamPol.Meta.ID ||
		i.Message != "acme/m2 is not allowed by team "+b.team.Meta.Name+"'s policy platform-default; requests for it are refused" {
		t.Fatalf("issue = %+v", i)
	}
	if issueAt(h.issues(KindPolicy, pol.Meta.ID), IssueModelRefBlocked, "spec.models[0]") != nil {
		t.Error("m1, which the team allows, is reported blocked")
	}

	pb := clone(b.w.pbs[len(b.w.pbs)-1])
	pb.Spec.AccountIDs = append(pb.Spec.AccountIDs, b.w.projectAccount(proj2).Meta.ID)
	apply(h, pb)
	i = issueAt(h.issues(KindPolicy, pol.Meta.ID), IssueModelRefBlocked, "spec.models[1]")
	if i == nil || !strings.HasSuffix(i.Message, "for 1 of the 2 accounts bound to this policy") {
		t.Errorf("issue = %+v, want the count", i)
	}
}
