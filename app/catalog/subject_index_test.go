package catalog

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// tenancyCatalog wires a reloaded catalog from the supplied rows.
func tenancyCatalog(t *testing.T, teams []*team.Team, projects []*project.Project,
	pols []*policy.Policy, keys []*key.Key, groups []*group.Group) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList(pols), modList{}, keyList{}, rlList{},
		rkList(keys), rcList{}, bndList{})
	c.UseTenancy(teamList(teams), projList(projects), saList{}, grpList(groups),
		roleList{}, rbList{}, pbList{})
	must(t, c.Reload(context.Background()))
	return c
}

// tenancySnap builds a catalog with one team, one project and one policy,
// reloaded and ready for the incremental Apply paths.
func tenancySnap(t *testing.T, keys ...*key.Key) (*Catalog, *team.Team, *project.Project, *policy.Policy) {
	t.Helper()
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "search"}}
	proj.Spec.TeamID = tm.Meta.ID
	proj.StampOwner()
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}}}

	c := tenancyCatalog(t, []*team.Team{tm}, []*project.Project{proj}, []*policy.Policy{pol}, keys, nil)
	return c, tm, proj, pol
}

// A project write that is not a rename leaves every key's subject list alone; a rename rewrites them, since the project slug is part of a service account's subjects.
func TestProjectUpsertReindexesSubjectsOnlyOnRename(t *testing.T) {
	f := newDisabledPolicyFixture(true)
	c := f.catalog(t)
	subjects := func(project string) []string {
		return []string{"serviceaccount:" + f.sa.Meta.ID, SubjectServiceAccounts, SubjectServiceAccounts + ":" + project, SubjectAuthenticated}
	}
	before := c.Current().SubjectsForKey(f.key.Meta.ID)
	if !slices.Equal(before, subjects("ml")) {
		t.Fatalf("subjects = %v, want %v", before, subjects("ml"))
	}
	touched := *f.project
	touched.Meta.DisplayName = "ML (display only)"
	must(t, c.ApplyProjectUpsert(&touched))
	if after := c.Current().SubjectsForKey(f.key.Meta.ID); &after[0] != &before[0] {
		t.Fatal("a non-rename project write rebuilt the key's subject list")
	}

	renamed := *f.project
	renamed.Meta.Name = "ml-2"
	must(t, c.ApplyProjectUpsert(&renamed))
	if got := c.Current().SubjectsForKey(f.key.Meta.ID); !slices.Equal(got, subjects("ml-2")) {
		t.Fatalf("subjects after the rename = %v, want %v", got, subjects("ml-2"))
	}
	if _, ok := c.Current().projectsByName["ml"]; ok {
		t.Error("the old project name still resolves")
	}
}

// keysByPrincipal must stay in step with keysByID across insert, update and
// delete, or a group write reindexes the wrong keys.
func TestKeysByPrincipalIndexTracksWrites(t *testing.T) {
	user := meta.NewID()
	k := userKeyFor(user)

	c, _, _, _ := tenancySnap(t, k)
	pk := principalKey(key.PrincipalUser, user)
	if got := len(c.Current().keysByPrincipal[pk]); got != 1 {
		t.Fatalf("index holds %d keys after build, want 1", got)
	}

	updated := *k
	updated.Spec.KeyHash = hex64("b")
	must(t, c.ApplyKeyUpsert(&updated))
	if got := c.Current().keysByPrincipal[pk]; len(got) != 1 || got[0].Spec.KeyHash != hex64("b") {
		t.Fatalf("index after an update = %v, want the updated key alone", got)
	}

	must(t, c.ApplyKeyDelete(k.Meta.ID))
	if got := len(c.Current().keysByPrincipal[pk]); got != 0 {
		t.Fatalf("index holds %d keys after a delete, want 0", got)
	}
}

// The index is what a group write walks; the subjects it produces must match
// what a full rebuild would.
func TestGroupWriteReindexesMemberSubjects(t *testing.T) {
	user := meta.NewID()
	k := userKeyFor(user)

	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	g.Spec.MemberIDs = []string{user}

	c := tenancyCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
	member := []string{"user:" + user, "group:data-science", SubjectAuthenticated}
	if subs := c.Current().SubjectsForKey(k.Meta.ID); !slices.Equal(subs, member) {
		t.Fatalf("subjects = %v, want %v", subs, member)
	}
	dropped := *g
	dropped.Spec.MemberIDs = []string{meta.NewID()}
	must(t, c.ApplyGroupUpsert(&dropped))
	if subs, want := c.Current().SubjectsForKey(k.Meta.ID), []string{"user:" + user, SubjectAuthenticated}; !slices.Equal(subs, want) {
		t.Fatalf("subjects = %v, want the membership dropped: %v", subs, want)
	}
}

// Snapshot slices are shared across clones, so a reindex must replace
// the slice rather than write through it.
func TestCloneDoesNotShareMutatedSubjectSlices(t *testing.T) {
	user := meta.NewID()
	k := userKeyFor(user)
	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "g1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	g.Spec.MemberIDs = []string{user}

	c := tenancyCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
	old := c.Current()
	oldSubs := slices.Clone(old.SubjectsForKey(k.Meta.ID))
	must(t, c.ApplyGroupDelete(g.Meta.ID))
	if got := old.SubjectsForKey(k.Meta.ID); !slices.Equal(got, oldSubs) {
		t.Fatalf("the published snapshot's subject list changed under it: %v, want %v", got, oldSubs)
	}
	if got, want := c.Current().SubjectsForKey(k.Meta.ID), []string{"user:" + user, SubjectAuthenticated}; !slices.Equal(got, want) {
		t.Fatalf("subjects after the group delete = %v, want %v", got, want)
	}
}

// userKeyFor is a key user owns and acts as.
func userKeyFor(user string) *key.Key {
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerUser, ID: user}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: user}, KeyHash: hex64("a")},
	}
}

// hex64 renders a 64-char hex string, the shape Key.Spec.KeyHash validates.
func hex64(seed string) string { return strings.Repeat(seed[:1], 64) }
