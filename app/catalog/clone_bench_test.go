package catalog

import (
	"context"
	"strconv"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
)

// benchKeys builds n user-principal keys across m groups, the shape a
// tenanted deployment's snapshot carries.
func benchKeys(n int) ([]*key.Key, []*group.Group) {
	const groups = 32
	users := make([]string, n)
	keys := make([]*key.Key, 0, n)
	for i := 0; i < n; i++ {
		users[i] = meta.NewID()
		k := &key.Key{Meta: meta.Metadata{
			ID: meta.NewID(), Name: "k" + strconv.Itoa(i),
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: users[i]},
		}}
		k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: users[i]}
		k.Spec.KeyHash = benchHash(i)
		keys = append(keys, k)
	}
	gs := make([]*group.Group, 0, groups)
	for g := 0; g < groups; g++ {
		grp := &group.Group{Meta: meta.Metadata{
			ID: meta.NewID(), Name: "g" + strconv.Itoa(g), Owner: meta.Owner{Kind: meta.OwnerSystem},
		}}
		for i := g; i < n; i += groups {
			grp.Spec.MemberIDs = append(grp.Spec.MemberIDs, users[i])
		}
		gs = append(gs, grp)
	}
	return keys, gs
}

func benchHash(i int) string {
	s := strconv.Itoa(i)
	out := make([]byte, 64)
	for j := range out {
		out[j] = 'a'
	}
	copy(out[64-len(s):], s)
	return string(out)
}

func benchCatalog(b *testing.B, n int) *Catalog {
	b.Helper()
	keys, groups := benchKeys(n)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "search"}}
	proj.Spec.TeamID = tm.Meta.ID
	proj.StampOwner()

	c := New(provList{}, hostList{}, polList{}, modList{}, keyList{}, rlList{},
		rkList(keys), rcList{}, bndList{})
	c.UseTenancy(teamList{tm}, projList{proj}, saList{}, grpList(groups),
		roleList{}, rbList{}, pbList{})
	if err := c.Reload(context.Background()); err != nil {
		b.Fatalf("reload: %v", err)
	}
	return c
}

// BenchmarkSnapshotClone10kKeys is what every incremental reconcile pays.
func BenchmarkSnapshotClone10kKeys(b *testing.B) {
	c := benchCatalog(b, 10000)
	snap := c.Current()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = snap.clone()
	}
}

// BenchmarkGroupUpsert10kKeys is the write that used to walk every key to
// reindex subjects.
func BenchmarkGroupUpsert10kKeys(b *testing.B) {
	c := benchCatalog(b, 10000)
	g := c.Current().AllGroups()[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next := *g
		if err := c.ApplyGroupUpsert(&next); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProjectUpsert10kKeys is the non-rename project write.
func BenchmarkProjectUpsert10kKeys(b *testing.B) {
	c := benchCatalog(b, 10000)
	p := c.Current().AllProjects()[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next := *p
		next.Meta.DisplayName = "d" + strconv.Itoa(i)
		if err := c.ApplyProjectUpsert(&next); err != nil {
			b.Fatal(err)
		}
	}
}
