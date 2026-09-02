package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
)

// bughuntCatalog wires a reloaded catalog from the supplied rows.
func bughuntCatalog(t *testing.T, teams []*team.Team, projects []*project.Project,
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

	c := bughuntCatalog(t, []*team.Team{tm}, []*project.Project{proj}, []*policy.Policy{pol}, keys, nil)
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

	c := bughuntCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
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

	c := bughuntCatalog(t, nil, nil, nil, []*key.Key{k}, []*group.Group{g})
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

// A rotated key's previous hash is indexed only while its grace window is
// open. With an injected clock the transition is reachable without sleeping.
func TestGraceWindowIndexUsesTheCatalogClock(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k1", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	k.Spec.KeyHash = hex64("c")
	k.Spec.PreviousKeyHash = hex64("d")
	until := base.Add(time.Hour)
	k.Spec.GraceUntil = &until

	c := New(provList{}, hostList{}, polList{}, modList{}, keyList{}, rlList{},
		rkList{k}, rcList{}, bndList{})
	c.UseClock(func() time.Time { return now })
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, prev := c.Current().KeyByHash(hex64("d")); got == nil || !prev {
		t.Fatal("previous hash is not indexed inside the grace window")
	}

	now = until.Add(time.Second)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := c.Current().KeyByHash(hex64("d")); got != nil {
		t.Fatal("previous hash still indexed after the grace window closed")
	}
}

// PolicyBinding subject keys are precomputed at build and at every
// incremental write, so the request path never renders them.
func TestPolicyBindingSubjectKeysArePrecomputed(t *testing.T) {
	c, _, proj, pol := tenancySnap(t)
	b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "b1"}}
	b.Spec.ProjectID = proj.Meta.ID
	b.Spec.PolicyID = pol.Meta.ID
	b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "data-science"}}
	b.StampOwner()

	if err := c.ApplyPolicyBindingUpsert(b); err != nil {
		t.Fatalf("apply binding: %v", err)
	}
	got := c.Current().PolicyBindingsForProject(proj.Meta.ID)
	if len(got) != 1 {
		t.Fatalf("bindings = %d, want 1", len(got))
	}
	if len(got[0].SubjectKeys) != 1 || got[0].SubjectKeys[0] != b.Spec.Subjects[0].Key() {
		t.Fatalf("SubjectKeys = %v, want %q", got[0].SubjectKeys, b.Spec.Subjects[0].Key())
	}
}

// A policy binding write must not disturb the allowed-combo sets — it names
// no provider, host or model.
func TestPolicyBindingWriteLeavesGrantsAlone(t *testing.T) {
	c, _, proj, pol := tenancySnap(t)
	before := c.Current().allowedCombosByPolicy[pol.Meta.ID]

	b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "b1"}}
	b.Spec.ProjectID = proj.Meta.ID
	b.Spec.PolicyID = pol.Meta.ID
	b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "g"}}
	b.StampOwner()
	if err := c.ApplyPolicyBindingUpsert(b); err != nil {
		t.Fatalf("apply binding: %v", err)
	}
	if got := c.Current().allowedCombosByPolicy[pol.Meta.ID]; len(got) != len(before) {
		t.Fatalf("allowed combos changed on a policy-binding write: %d → %d", len(before), len(got))
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
