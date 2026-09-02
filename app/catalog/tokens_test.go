package catalog

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// versionList is the snapshot's token-version source in tests.
type versionList map[string]int

func (v versionList) TokenVersions(context.Context) (map[string]int, error) { return v, nil }

// userKey is a personal key for one of the fixture's users.
func userKey(userID, keyHash string) *key.Key {
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: userID}, KeyHash: keyHash},
	}
}

func TestBuild_SubjectsByKey(t *testing.T) {
	f := newSubjectsFixture()
	alicesKey := userKey(f.alice, hash("b"))
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{},
		rkList{f.k, alicesKey}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{f.group},
		roleList{}, rbList{}, pbList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	want := []string{
		"serviceaccount:" + f.sa.Meta.ID,
		SubjectServiceAccounts,
		SubjectServiceAccounts + ":ml-search",
		SubjectAuthenticated,
	}
	if got := s.SubjectsForKey(f.k.Meta.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("service-account key subjects = %v, want %v", got, want)
	}
	wantUser := []string{"user:" + f.alice, "group:data-science", SubjectAuthenticated}
	if got := s.SubjectsForKey(alicesKey.Meta.ID); !reflect.DeepEqual(got, wantUser) {
		t.Errorf("user key subjects = %v, want %v", got, wantUser)
	}
}

// TestApplyGroupUpsert_ReindexesKeySubjects covers the invalidation half:
// membership changes must reach the precomputed lists, in both directions.
func TestApplyGroupUpsert_ReindexesKeySubjects(t *testing.T) {
	f := newSubjectsFixture()
	bobsKey := userKey(f.bob, hash("c"))
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{},
		rkList{bobsKey}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{f.group},
		roleList{}, rbList{}, pbList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := c.Current().SubjectsForKey(bobsKey.Meta.ID); len(got) != 3 {
		t.Fatalf("bob's subjects = %v, want the group membership included", got)
	}

	dropped := *f.group
	dropped.Spec.MemberIDs = []string{f.alice}
	if err := c.ApplyGroupUpsert(&dropped); err != nil {
		t.Fatalf("group upsert: %v", err)
	}
	want := []string{"user:" + f.bob, SubjectAuthenticated}
	if got := c.Current().SubjectsForKey(bobsKey.Meta.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("after removal subjects = %v, want %v", got, want)
	}

	if err := c.ApplyGroupUpsert(f.group); err != nil {
		t.Fatalf("group re-upsert: %v", err)
	}
	if got := c.Current().SubjectsForKey(bobsKey.Meta.ID); len(got) != 3 {
		t.Errorf("after re-adding subjects = %v, want the group back", got)
	}
}

// TestApplyProjectUpsert_ReindexesKeySubjects pins the project slug that
// rides a service account's subjects through a rename.
func TestApplyProjectUpsert_ReindexesKeySubjects(t *testing.T) {
	f := newSubjectsFixture()
	c := f.catalog(t)

	renamed := *f.project
	renamed.Meta.Name = "ml-ranking"
	if err := c.ApplyProjectUpsert(&renamed); err != nil {
		t.Fatalf("project upsert: %v", err)
	}
	want := SubjectServiceAccounts + ":ml-ranking"
	got := c.Current().SubjectsForKey(f.k.Meta.ID)
	if len(got) < 3 || got[2] != want {
		t.Errorf("subjects after rename = %v, want %q among them", got, want)
	}
}

func TestSnapshot_TokenVersions(t *testing.T) {
	f := newSubjectsFixture()
	versions := versionList{f.alice: 3}
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{f.group},
		roleList{}, rbList{}, pbList{})
	c.UseTokenVersions(versions)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if v, ok := c.Current().TokenVersion(f.alice); !ok || v != 3 {
		t.Errorf("TokenVersion(alice) = (%d, %v), want (3, true)", v, ok)
	}
	// An unknown user reads as absent, which rejects any token claiming to
	// be theirs.
	if _, ok := c.Current().TokenVersion(f.bob); ok {
		t.Error("TokenVersion(bob) resolved, want absent")
	}

	// A users write reaches the snapshot without a full reload.
	versions[f.alice] = 4
	if err := c.ReloadTokenVersions(context.Background()); err != nil {
		t.Fatalf("ReloadTokenVersions: %v", err)
	}
	if v, _ := c.Current().TokenVersion(f.alice); v != 4 {
		t.Errorf("TokenVersion(alice) after bump = %d, want 4", v)
	}
}
