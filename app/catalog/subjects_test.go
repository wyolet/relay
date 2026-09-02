package catalog

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// subjectsFixture is one team → one project → one service account → one
// key, plus a second account whose project is absent and a group holding
// two users.
type subjectsFixture struct {
	team    *team.Team
	project *project.Project
	pol     *policy.Policy
	sa      *serviceaccount.ServiceAccount
	orphan  *serviceaccount.ServiceAccount
	badPol  *serviceaccount.ServiceAccount
	k       *key.Key
	group   *group.Group
	alice   string
	bob     string
}

func hash(seed string) string {
	return strings.Repeat(seed, 64/len(seed))
}

func newSubjectsFixture() subjectsFixture {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}}}

	newSA := func(name, projectID, policyID string) *serviceaccount.ServiceAccount {
		sa := &serviceaccount.ServiceAccount{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: serviceaccount.Spec{ProjectID: projectID, PolicyID: policyID},
		}
		sa.StampOwner()
		return sa
	}
	sa := newSA("indexer", proj.Meta.ID, "")
	orphan := newSA("orphan", meta.NewID(), "")
	badPol := newSA("bad-policy", proj.Meta.ID, meta.NewID())

	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID},
			KeyHash:   hash("a"),
		},
	}

	alice, bob := meta.NewID(), meta.NewID()
	g := &group.Group{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{alice, bob}},
	}

	return subjectsFixture{
		team: tm, project: proj, pol: pol,
		sa: sa, orphan: orphan, badPol: badPol,
		k: k, group: g, alice: alice, bob: bob,
	}
}

func (f subjectsFixture) catalog(t *testing.T) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{f.k}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project},
		saList{f.sa, f.orphan, f.badPol}, grpList{f.group})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

func TestBuild_ServiceAccountAndKeyMembership(t *testing.T) {
	f := newSubjectsFixture()
	s := f.catalog(t).Current()

	if _, ok := s.ServiceAccount(f.sa.Meta.ID); !ok {
		t.Fatal("service account with a live project should be in the snapshot")
	}
	if _, ok := s.ServiceAccount(f.orphan.Meta.ID); ok {
		t.Error("service account whose project is absent should be dropped")
	}
	if _, ok := s.ServiceAccount(f.badPol.Meta.ID); ok {
		t.Error("service account whose policy override does not resolve should be dropped")
	}
	if got := s.ServiceAccountsForProject(f.project.Meta.ID); len(got) != 1 || got[0].Meta.Name != "indexer" {
		t.Errorf("ServiceAccountsForProject = %v, want [indexer]", got)
	}
	if k, _ := s.KeyByHash(hash("a")); k == nil {
		t.Error("key whose principal resolves should be in the hash index")
	}
}

func TestBuild_KeyDroppedWhenPrincipalAbsent(t *testing.T) {
	f := newSubjectsFixture()
	f.k.Spec.Principal.ID = meta.NewID() // points at no account
	s := f.catalog(t).Current()

	if k, _ := s.KeyByHash(hash("a")); k != nil {
		t.Error("key whose service account is absent should be dropped")
	}
}

func TestSnapshot_GroupsForUser(t *testing.T) {
	f := newSubjectsFixture()
	s := f.catalog(t).Current()

	if got := s.GroupsForUser(f.alice); !reflect.DeepEqual(got, []string{"data-science"}) {
		t.Errorf("GroupsForUser(alice) = %v, want [data-science]", got)
	}
	if got := s.GroupsForUser(meta.NewID()); len(got) != 0 {
		t.Errorf("GroupsForUser(stranger) = %v, want empty", got)
	}
	if _, ok := s.GroupByName("data-science"); !ok {
		t.Error("GroupByName should find the seeded group")
	}
}

func TestKeyByHash_GraceWindow(t *testing.T) {
	f := newSubjectsFixture()
	future := time.Now().Add(time.Hour)
	f.k.Spec.PreviousKeyHash = hash("b")
	f.k.Spec.GraceUntil = &future
	c := f.catalog(t)

	k, matchedPrevious := c.Current().KeyByHash(hash("b"))
	if k == nil || !matchedPrevious {
		t.Fatalf("previous hash in grace: got (%v, %v), want (key, true)", k, matchedPrevious)
	}
	if k, matchedPrevious := c.Current().KeyByHash(hash("a")); k == nil || matchedPrevious {
		t.Fatalf("current hash: got (%v, %v), want (key, false)", k, matchedPrevious)
	}

	// Once the window closes the previous hash leaves the index on the next
	// reconcile.
	past := time.Now().Add(-time.Minute)
	f.k.Spec.GraceUntil = &past
	if err := c.ApplyKeyUpsert(f.k); err != nil {
		t.Fatalf("ApplyKeyUpsert: %v", err)
	}
	if k, _ := c.Current().KeyByHash(hash("b")); k != nil {
		t.Error("previous hash should be gone once the grace window closes")
	}
	if k, _ := c.Current().KeyByHash(hash("a")); k == nil {
		t.Error("current hash must survive the grace expiry")
	}
}

func TestApplyServiceAccountDelete_EvictsKeys(t *testing.T) {
	f := newSubjectsFixture()
	c := f.catalog(t)

	if err := c.ApplyServiceAccountDelete(f.sa.Meta.ID); err != nil {
		t.Fatalf("ApplyServiceAccountDelete: %v", err)
	}
	s := c.Current()
	if _, ok := s.ServiceAccount(f.sa.Meta.ID); ok {
		t.Error("deleted service account still present")
	}
	if k, _ := s.KeyByHash(hash("a")); k != nil {
		t.Error("key of a deleted service account should be evicted")
	}
}

func TestApplyProjectDelete_EvictsServiceAccountsAndKeys(t *testing.T) {
	f := newSubjectsFixture()
	c := f.catalog(t)

	if err := c.ApplyProjectDelete(f.project.Meta.ID); err != nil {
		t.Fatalf("ApplyProjectDelete: %v", err)
	}
	s := c.Current()
	if _, ok := s.ServiceAccount(f.sa.Meta.ID); ok {
		t.Error("service account of a deleted project should be evicted")
	}
	if k, _ := s.KeyByHash(hash("a")); k != nil {
		t.Error("key reached through the account should be evicted too")
	}
}

func TestApplyGroupUpsert_UpdatesGroupsForUser(t *testing.T) {
	f := newSubjectsFixture()
	c := f.catalog(t)

	carol := meta.NewID()
	updated := *f.group
	updated.Spec.MemberIDs = []string{f.alice, carol}
	if err := c.ApplyGroupUpsert(&updated); err != nil {
		t.Fatalf("ApplyGroupUpsert: %v", err)
	}
	s := c.Current()
	if got := s.GroupsForUser(carol); !reflect.DeepEqual(got, []string{"data-science"}) {
		t.Errorf("GroupsForUser(carol) = %v, want [data-science]", got)
	}
	if got := s.GroupsForUser(f.bob); len(got) != 0 {
		t.Errorf("GroupsForUser(bob) = %v, want empty after removal", got)
	}

	if err := c.ApplyGroupDelete(f.group.Meta.ID); err != nil {
		t.Fatalf("ApplyGroupDelete: %v", err)
	}
	if got := c.Current().GroupsForUser(f.alice); len(got) != 0 {
		t.Errorf("GroupsForUser(alice) = %v, want empty after group delete", got)
	}
}
