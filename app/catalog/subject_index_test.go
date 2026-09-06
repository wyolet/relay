package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
)

// tenancyCatalog wires a reloaded catalog from the supplied rows.
func tenancyCatalog(t *testing.T, teams []*team.Team, projects []*project.Project,
	pols []*policy.Policy, keys []*key.Key, groups []*group.Group) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList(pols), modList{}, keyList{}, rlList{},
		rkList(keys), rcList{}, bndList{})
	c.UseTenancy(teamList(teams), projList(projects), saList{}, grpList(groups),
		roleList{}, rbList{}, pbList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
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

// A project write that is not a rename leaves every key's subject list
// alone; only a rename can change them (the project slug is part of a
// service account's subjects).
func TestProjectUpsertReindexesSubjectsOnlyOnRename(t *testing.T) {
	c, _, proj, _ := tenancySnap(t)
	before := c.Current()

	touched := *proj
	touched.Meta.DisplayName = "Search (renamed display only)"
	if err := c.ApplyProjectUpsert(&touched); err != nil {
		t.Fatalf("apply project: %v", err)
	}
	after := c.Current()
	if len(after.subjectsByKey) != len(before.subjectsByKey) {
		t.Fatalf("subject index size changed on a non-rename write")
	}

	renamed := *proj
	renamed.Meta.Name = "search-2"
	if err := c.ApplyProjectUpsert(&renamed); err != nil {
		t.Fatalf("apply rename: %v", err)
	}
	if _, ok := c.Current().projectsByName["search-2"]; !ok {
		t.Fatal("rename did not land")
	}
}

// keysByPrincipal must stay in step with keysByID across insert, update and
// delete, or a group write reindexes the wrong keys.
func TestKeysByPrincipalIndexTracksWrites(t *testing.T) {
	user := meta.NewID()
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerUser, ID: user}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: user}
	k.Spec.KeyHash = hex64("a")

	c, _, _, _ := tenancySnap(t, k)
	pk := principalKey(key.PrincipalUser, user)
	if got := len(c.Current().keysByPrincipal[pk]); got != 1 {
		t.Fatalf("index holds %d keys after build, want 1", got)
	}

	updated := *k
	updated.Spec.KeyHash = hex64("b")
	if err := c.ApplyKeyUpsert(&updated); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := len(c.Current().keysByPrincipal[pk]); got != 1 {
		t.Fatalf("index holds %d keys after an update, want 1", got)
	}

	if err := c.ApplyKeyDelete(k.Meta.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := len(c.Current().keysByPrincipal[pk]); got != 0 {
		t.Fatalf("index holds %d keys after a delete, want 0", got)
	}
}

// The index is what a group write walks; the subjects it produces must match
// what a full rebuild would.
func TestGroupWriteReindexesMemberSubjects(t *testing.T) {
	user := meta.NewID()
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerUser, ID: user}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: user}
	k.Spec.KeyHash = hex64("a")

	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	g.Spec.MemberIDs = []string{user}

	c := tenancyCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
	subs := c.Current().SubjectsForKey(k.Meta.ID)
	if !contains(subs, "group:data-science") {
		t.Fatalf("subjects = %v, want the group membership", subs)
	}

	dropped := *g
	dropped.Spec.MemberIDs = []string{meta.NewID()}
	if err := c.ApplyGroupUpsert(&dropped); err != nil {
		t.Fatalf("apply group: %v", err)
	}
	if subs := c.Current().SubjectsForKey(k.Meta.ID); contains(subs, "group:data-science") {
		t.Fatalf("subjects = %v, want the membership dropped", subs)
	}
}

// Snapshot slices are shared across clones now, so a reindex must replace
// the slice rather than write through it.
func TestCloneDoesNotShareMutatedSubjectSlices(t *testing.T) {
	user := meta.NewID()
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerUser, ID: user}}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: user}
	k.Spec.KeyHash = hex64("a")
	g := &group.Group{Meta: meta.Metadata{ID: meta.NewID(), Name: "g1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	g.Spec.MemberIDs = []string{user}

	c := tenancyCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
	old := c.Current()
	oldSubs := append([]string(nil), old.SubjectsForKey(k.Meta.ID)...)

	dropped := *g
	dropped.Spec.MemberIDs = nil
	if err := c.ApplyGroupDelete(g.Meta.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	got := old.SubjectsForKey(k.Meta.ID)
	if len(got) != len(oldSubs) {
		t.Fatalf("the published snapshot's subject list changed under it: %v, want %v", got, oldSubs)
	}
}

// hex64 renders a 64-char hex string, the shape Key.Spec.KeyHash validates.
func hex64(seed string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = seed[0]
	}
	return string(out)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
