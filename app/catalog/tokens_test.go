package catalog

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
)

// versionList is the snapshot's token-version source in tests.
type versionList map[string]int

func (v versionList) TokenVersionsAndDisabled(context.Context) (map[string]int, map[string]struct{}, error) {
	return v, nil, nil
}

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

// disabledList is a users source whose disabled ids a test can change between reloads.
type disabledList struct{ disabled map[string]struct{} }

func (d *disabledList) TokenVersionsAndDisabled(context.Context) (map[string]int, map[string]struct{}, error) {
	return map[string]int{}, d.disabled, nil
}

// UserDisabled reads the users source on both reload paths, survives a copy-on-write apply, and is false for an id with no row and for a catalog with no source.
func TestSnapshot_UserDisabled(t *testing.T) {
	f := newSubjectsFixture()
	if f.catalog(t).Current().UserDisabled(f.alice) {
		t.Fatal("UserDisabled with no users source = true, want false")
	}

	users := &disabledList{disabled: map[string]struct{}{f.alice: {}}}
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{f.group},
		roleList{}, rbList{}, pbList{})
	c.UseTokenVersions(users)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !c.Current().UserDisabled(f.alice) {
		t.Error("UserDisabled(alice) after Reload = false, want true")
	}
	if c.Current().UserDisabled(f.bob) {
		t.Error("UserDisabled(bob), an id with no row, = true, want false")
	}

	renamed := *f.project
	renamed.Meta.Name = "ml-ranking"
	if err := c.ApplyProjectUpsert(&renamed); err != nil {
		t.Fatalf("project upsert: %v", err)
	}
	if !c.Current().UserDisabled(f.alice) {
		t.Error("UserDisabled(alice) after a copy-on-write apply = false, want true")
	}

	users.disabled = nil
	if err := c.ReloadTokenVersions(context.Background()); err != nil {
		t.Fatalf("ReloadTokenVersions: %v", err)
	}
	if c.Current().UserDisabled(f.alice) {
		t.Error("UserDisabled(alice) after re-enable = true, want false")
	}
}

// blockingVersionLister lets a test hold TokenVersions open so it can probe
// whether c.rmu is already held while the read is in flight. A full Reload
// also consults the lister when one is configured, so a second call must
// not re-close entered — only the first call's arrival matters to the test.
type blockingVersionLister struct {
	entered   chan struct{}
	closeOnce sync.Once
	release   chan struct{}
}

func (b *blockingVersionLister) TokenVersionsAndDisabled(context.Context) (map[string]int, map[string]struct{}, error) {
	b.closeOnce.Do(func() { close(b.entered) })
	<-b.release
	return map[string]int{}, nil, nil
}

// TestReloadTokenVersions_ReadHappensUnderTheLock proves the version read
// is inside c.rmu, not before it: a full Reload racing the read would
// otherwise land in between and get clobbered by the older versions this
// call is about to publish. While TokenVersions is blocked mid-call, a
// concurrent Reload (which also needs c.rmu) must not be able to complete.
func TestReloadTokenVersions_ReadHappensUnderTheLock(t *testing.T) {
	f := newSubjectsFixture()
	lister := &blockingVersionLister{entered: make(chan struct{}), release: make(chan struct{})}
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{f.group},
		roleList{}, rbList{}, pbList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	c.UseTokenVersions(lister)

	reloadDone := make(chan error, 1)
	go func() {
		<-lister.entered // wait until the version read is in flight
		reloadDone <- c.Reload(context.Background())
	}()

	go func() { _ = c.ReloadTokenVersions(context.Background()) }()

	<-lister.entered
	select {
	case err := <-reloadDone:
		t.Fatalf("a concurrent Reload completed (err %v) while TokenVersions was still being read — the read is not under c.rmu", err)
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as it must be: Reload needs the same c.rmu the
		// in-flight read is holding.
	}

	close(lister.release)
	if err := <-reloadDone; err != nil {
		t.Fatalf("Reload after release: %v", err)
	}
}
