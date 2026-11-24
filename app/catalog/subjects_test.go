package catalog

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
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

	newSA := func(name, projectID string) *serviceaccount.ServiceAccount {
		sa := &serviceaccount.ServiceAccount{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: serviceaccount.Spec{ProjectID: projectID},
		}
		sa.StampOwner()
		return sa
	}
	sa := newSA("indexer", proj.Meta.ID)
	orphan := newSA("orphan", meta.NewID())

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
		sa: sa, orphan: orphan,
		k: k, group: g, alice: alice, bob: bob,
	}
}

func (f subjectsFixture) catalog(t *testing.T) *Catalog { return f.catalogWith(t, rkList{f.k}, nil) }

// catalogWith loads the fixture's tenancy with the given keys and, when users is set, that users source.
func (f subjectsFixture) catalogWith(t *testing.T, keys rkList, users TokenVersionLister) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, keys, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project},
		saList{f.sa, f.orphan}, grpList{f.group}, roleList{}, rbList{}, pbList{})
	if users != nil {
		c.UseTokenVersions(users)
	}
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
	if got := s.ServiceAccountsForProject(f.project.Meta.ID); len(got) != 1 || got[0].Meta.Name != "indexer" {
		t.Errorf("ServiceAccountsForProject = %v, want [indexer]", got)
	}
	if k, _ := s.KeyByHash(hash("a")); k == nil {
		t.Error("key whose principal resolves should be in the hash index")
	}
}

// A service account a user owns is kept and indexed under the user, carries no group of its owner, and its keys resolve; deleting it takes it from the index.
func TestBuild_UserServiceAccount(t *testing.T) {
	f := newSubjectsFixture()
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.alice}}}
	bot.StampOwner()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-bot-key", Owner: bot.Meta.Owner},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: bot.Meta.ID}, KeyHash: hash("c")},
	}
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{f.k, k}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa, bot}, grpList{f.group}, roleList{}, rbList{}, pbList{})
	must(t, c.Reload(context.Background()))
	s := c.Current()

	if got := namesOf(s.ServiceAccountsForUser(f.alice), func(sa *serviceaccount.ServiceAccount) string { return sa.Meta.Name }); !slices.Equal(got, []string{"alice-bot"}) {
		t.Errorf("ServiceAccountsForUser = %v, want [alice-bot]", got)
	}
	want := []string{"serviceaccount:" + bot.Meta.ID, SubjectServiceAccounts, SubjectAuthenticated}
	if got := s.SubjectsForServiceAccount(bot.Meta.ID); !slices.Equal(got, want) {
		t.Errorf("SubjectsForServiceAccount = %v, want %v", got, want)
	}
	if got, _ := s.KeyByHash(hash("c")); got == nil {
		t.Error("key of a user's service account should be in the hash index")
	}
	must(t, c.ApplyServiceAccountDelete(bot.Meta.ID))
	if got := c.Current().ServiceAccountsForUser(f.alice); len(got) != 0 {
		t.Errorf("ServiceAccountsForUser after its delete = %v, want none", got)
	}
}

// teamRows is a team source whose rows a test changes after the catalog loads, as the store's would.
type teamRows []*team.Team

func (r *teamRows) List(context.Context) ([]*team.Team, error) { return *r, nil }

// A service account a team owns directly is kept while its team is, indexed under the team and not under any project, carries the team's group subject, and its keys resolve; one whose team is absent is dropped. Deleting the team evicts it as a fresh build without the team would.
func TestBuild_TeamServiceAccount(t *testing.T) {
	f := newSubjectsFixture()
	teamOwned := func(name, teamID string) *serviceaccount.ServiceAccount {
		sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}}
		sa.StampOwner()
		return sa
	}
	deployer, stray := teamOwned("deployer", f.team.Meta.ID), teamOwned("stray", meta.NewID())
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer-prod", Owner: deployer.Meta.Owner},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: deployer.Meta.ID}, KeyHash: hash("b")},
	}
	load := func(teams teamList) *Catalog {
		c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{f.k, k}, rcList{}, bndList{})
		c.UseTenancy(teams, projList{f.project}, saList{f.sa, f.orphan, deployer, stray}, grpList{f.group}, roleList{}, rbList{}, pbList{})
		must(t, c.Reload(context.Background()))
		return c
	}
	c := load(teamList{f.team})
	s := c.Current()

	if _, ok := s.ServiceAccount(stray.Meta.ID); ok {
		t.Error("service account whose team is absent should be dropped")
	}
	if got := namesOf(s.ServiceAccountsForTeam(f.team.Meta.ID), func(sa *serviceaccount.ServiceAccount) string { return sa.Meta.Name }); !slices.Equal(got, []string{"deployer"}) {
		t.Errorf("ServiceAccountsForTeam = %v, want [deployer]", got)
	}
	if got := namesOf(s.ServiceAccountsForProject(f.project.Meta.ID), func(sa *serviceaccount.ServiceAccount) string { return sa.Meta.Name }); !slices.Equal(got, []string{"indexer"}) {
		t.Errorf("ServiceAccountsForProject = %v, want [indexer]", got)
	}
	want := []string{"serviceaccount:" + deployer.Meta.ID, SubjectServiceAccounts, SubjectServiceAccounts + ":team:platform", SubjectAuthenticated}
	if got := s.SubjectsForServiceAccount(deployer.Meta.ID); !slices.Equal(got, want) {
		t.Errorf("SubjectsForServiceAccount = %v, want %v", got, want)
	}
	if got, _ := s.KeyByHash(hash("b")); got == nil {
		t.Error("key of a team's service account should be in the hash index")
	}

	must(t, c.ApplyServiceAccountDelete(deployer.Meta.ID))
	if got := c.Current().ServiceAccountsForTeam(f.team.Meta.ID); len(got) != 0 {
		t.Errorf("ServiceAccountsForTeam after its delete = %v, want none", got)
	}
	must(t, c.ApplyServiceAccountUpsert(deployer))
	if got := c.Current().ServiceAccountsForTeam(f.team.Meta.ID); len(got) != 1 {
		t.Errorf("ServiceAccountsForTeam after its upsert = %v, want [deployer]", got)
	}

	must(t, c.ApplyTeamDelete(f.team.Meta.ID))
	if _, ok := c.Current().ServiceAccount(deployer.Meta.ID); ok {
		t.Error("service account of a deleted team is still in the snapshot")
	}
	assertSameMembership(t, c.Current(), load(teamList{}).Current())
}

// Renaming a team reaches the keys of the service accounts it owns, whose subjects carry its slug; a new team taking the old name gets no key's subject. The reconciled snapshot matches a build from the same rows.
func TestApplyTeamRename_ReindexesItsServiceAccountKeys(t *testing.T) {
	f := newSubjectsFixture()
	deployer := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}}}
	deployer.StampOwner()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "deployer-prod", Owner: deployer.Meta.Owner},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: deployer.Meta.ID}, KeyHash: hash("b")},
	}
	load := func(teams TeamLister) *Catalog {
		c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{f.k, k}, rcList{}, bndList{})
		c.UseTenancy(teams, projList{f.project}, saList{f.sa, deployer}, grpList{f.group}, roleList{}, rbList{}, pbList{})
		must(t, c.Reload(context.Background()))
		return c
	}
	subjects := func(c *Catalog) []string { return c.Current().SubjectsForKey(k.Meta.ID) }
	other := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "staging", Owner: meta.Global}}
	stored := &teamRows{f.team, other}
	c := load(stored)

	renamed := *f.team
	renamed.Meta.Name = "infra"
	*stored = teamRows{&renamed, other}
	must(t, c.ApplyTeamUpsert(&renamed))
	want := []string{"serviceaccount:" + deployer.Meta.ID, SubjectServiceAccounts, SubjectServiceAccounts + ":team:infra", SubjectAuthenticated}
	if got := subjects(c); !slices.Equal(got, want) {
		t.Errorf("subjects after the rename = %v, want %v", got, want)
	}
	if got, built := subjects(c), subjects(load(teamList{&renamed, other})); !slices.Equal(got, built) {
		t.Errorf("subjects after the rename = %v, a build from the same rows gives %v", got, built)
	}

	// The source keeps the old row: a reload would undo this rename, so only the incremental path shows it.
	taker := *other
	taker.Meta.Name = f.team.Meta.Name
	must(t, c.ApplyTeamUpsert(&taker))
	if got, ok := c.Current().Team(other.Meta.ID); !ok || got.Meta.Name != f.team.Meta.Name {
		t.Fatalf("team after the rename = %+v, want it applied incrementally as %q", got, f.team.Meta.Name)
	}
	if got := subjects(c); !slices.Equal(got, want) {
		t.Errorf("subjects after another team took the old name = %v, want %v", got, want)
	}
	if got, built := subjects(c), subjects(load(teamList{&renamed, &taker})); !slices.Equal(got, built) {
		t.Errorf("subjects after another team took the old name = %v, a build from the same rows gives %v", got, built)
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

// The system: prefix names relay's own group subjects; an IdP asserting a
// group by that name must not slip a user into a reserved subject.
func TestIdPGroupsCannotClaimReservedSubjects(t *testing.T) {
	subs := UserSubjects("u-1", nil, []string{"system:serviceaccounts", "system:masters", "platform-eng"})
	for _, s := range subs {
		if s == SubjectServiceAccounts || s == "group:system:masters" {
			t.Fatalf("subjects = %v, carry the IdP-asserted %q", subs, s)
		}
	}
	if !slices.Contains(subs, "group:platform-eng") || !slices.Contains(subs, SubjectAuthenticated) {
		t.Fatalf("subjects = %v, want the ordinary IdP group and the authenticated catch-all", subs)
	}
}
