// reconcile_concurrent_test.go drives the COW reconciler from many goroutines at
// once. The reconciler's contract is that Apply*/Reload clone under rmu and
// publish with an atomic store, so a Current() holder keeps reading a
// consistent snapshot while writers churn. Only `-race` can observe a
// violation of that (a torn read is a benign-looking wrong answer without
// it), so these live behind `make test-race`.

package catalog

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

// raceVersionList hands back a fresh map per call: the snapshot keeps the
// map it is given, so a shared one would let a later Reload mutate what an
// older snapshot's readers are still walking.
type raceVersionList map[string]int

func (l raceVersionList) TokenVersionsAndDisabled(context.Context) (map[string]int, map[string]struct{}, error) {
	out := make(map[string]int, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out, nil, nil
}

// raceWorld is the row set the writers churn. Every row here is read-only
// after construction — an event upserts a *copy*, so a reader holding an
// older snapshot never sees a row mutate in place.
type raceWorld struct {
	cat *Catalog

	models  modList
	keys    keyList
	pols    polList
	rks     rkList
	team    *team.Team
	live    *project.Project
	dead    *project.Project
	role    *role.Role
	grp     *group.Group
	sa      *serviceaccount.ServiceAccount
	rbs     rbList
	pbs     pbList
	userIDs []string
}

func newRaceWorld(t *testing.T) *raceWorld {
	t.Helper()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	f := newRBACFixture()

	grp := &group.Group{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "platform-eng", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	sa := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"},
		Spec: serviceaccount.Spec{ProjectID: f.live.Meta.ID},
	}
	sa.StampOwner()

	allPols := append(polList{}, pols...)
	allPols = append(allPols, f.pol)

	c := New(provs, hosts, allPols, models, keys, rls, rks, rcList{}, bnds)
	c.UseTenancy(teamList{f.team}, projList{f.live, f.disabled}, saList{sa}, grpList{grp},
		roleList{f.role},
		rbList{f.global, f.teamWide, f.inProject, f.onDead},
		pbList{f.low, f.highA, f.noPol, f.onDeadP})
	users := []string{meta.NewID(), meta.NewID(), meta.NewID()}
	c.UseTokenVersions(raceVersionList{users[0]: 1, users[1]: 2, users[2]: 3})
	must(t, c.Reload(context.Background()))
	return &raceWorld{
		cat: c, models: models, keys: keys, pols: allPols, rks: rks,
		team: f.team, live: f.live, dead: f.disabled, role: f.role, grp: grp, sa: sa,
		rbs:     rbList{f.global, f.teamWide, f.inProject, f.onDead},
		pbs:     pbList{f.low, f.highA, f.noPol, f.onDeadP},
		userIDs: users,
	}
}

// events is the writer alphabet: every entry publishes a fresh copy of one row, so nothing a live snapshot points at is mutated after publication. Every row is valid, so every write must be accepted.
func (w *raceWorld) events() []func() error {
	c := w.cat
	on := true
	var out []func() error
	add := func(fns ...func() error) { out = append(out, fns...) }
	for _, m := range w.models {
		add(func() error { cp := *m; cp.Spec.Enabled = &on; return c.ApplyModelUpsert(&cp) },
			func() error { return c.ApplyModelDelete(m.Meta.ID) })
	}
	for _, k := range w.keys {
		add(func() error { cp := *k; cp.Spec.Enabled = &on; return c.ApplyHostKeyUpsert(&cp) },
			func() error { return c.ApplyHostKeyDelete(k.Meta.ID) })
	}
	for _, rk := range w.rks {
		add(func() error { cp := *rk; cp.Spec.Enabled = &on; return c.ApplyKeyUpsert(&cp) },
			func() error { return c.ApplyKeyDelete(rk.Meta.ID) })
	}
	for _, p := range w.pols {
		add(func() error { cp := *p; cp.Spec.Enabled = &on; return c.ApplyPolicyUpsert(&cp) })
	}
	// Tenancy churn: re-upsert the projects and the team, delete the disabled project, and delete and revive the service account whose keys hang off it.
	add(func() error { cp := *w.live; cp.Spec.Enabled = &on; return c.ApplyProjectUpsert(&cp) },
		func() error { cp := *w.dead; cp.Spec.Enabled = &on; return c.ApplyProjectUpsert(&cp) },
		func() error { return c.ApplyProjectDelete(w.dead.Meta.ID) },
		func() error { cp := *w.team; cp.Spec.Enabled = &on; return c.ApplyTeamUpsert(&cp) },
		func() error { cp := *w.sa; cp.Spec.Enabled = &on; return c.ApplyServiceAccountUpsert(&cp) },
		func() error { return c.ApplyServiceAccountDelete(w.sa.Meta.ID) })
	// Subject churn: group membership and binding subject lists are what the subject index is rebuilt from.
	for n := 0; n <= len(w.userIDs); n++ {
		members := append([]string(nil), w.userIDs[:n]...)
		add(func() error {
			cp := *w.grp
			cp.Spec.MemberIDs, cp.Spec.Enabled = members, &on
			return c.ApplyGroupUpsert(&cp)
		})
	}
	add(func() error { return c.ApplyGroupDelete(w.grp.Meta.ID) })
	for _, b := range w.rbs {
		add(func() error {
			cp := *b
			cp.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: w.grp.Meta.Name}}
			return c.ApplyRoleBindingUpsert(&cp)
		}, func() error { return c.ApplyRoleBindingDelete(b.Meta.ID) })
	}
	for _, b := range w.pbs {
		add(func() error {
			cp := *b
			cp.Spec.AccountIDs = []string{meta.NewID()}
			return c.ApplyPolicyBindingUpsert(&cp)
		}, func() error { return c.ApplyPolicyBindingDelete(b.Meta.ID) })
	}
	add(func() error { cp := *w.role; cp.Spec.Enabled = &on; return c.ApplyRoleUpsert(&cp) },
		// Whole-snapshot republishes racing the incremental ones.
		func() error { return c.Reload(context.Background()) },
		func() error { return c.ReloadTokenVersions(context.Background()) })
	return out
}

// readAccessors touches every index a data-plane reader reaches for. It
// asserts nothing: its job is to give the race detector something to catch
// a writer mutating a published snapshot in place.
func (w *raceWorld) readAccessors(s *Snapshot) int {
	n := 0
	n += len(s.AllModels()) + len(s.AllPolicies()) + len(s.AllKeys()) + len(s.AllHostKeys())
	n += len(s.AllTeams()) + len(s.AllProjects()) + len(s.AllServiceAccounts()) + len(s.AllGroups())
	n += len(s.AllRoles()) + len(s.AllRoleBindings()) + len(s.AllPolicyBindings())
	n += len(s.EnabledModels()) + len(s.EnabledHosts()) + len(s.AllBindings())
	for _, m := range w.models {
		n += len(s.BindingsForModel(m.Meta.ID))
		n += len(s.ModelsByName(m.Meta.Name))
		if _, ok := s.Model(m.Meta.ID); ok {
			n++
		}
	}
	for _, p := range w.pols {
		if got, ok := s.Policy(p.Meta.ID); ok {
			n += len(got.Spec.PoolIDs) + len(got.Spec.RateLimits)
		}
	}
	for _, rk := range w.rks {
		if k, _ := s.KeyByHash(rk.Spec.KeyHash); k != nil {
			n++
		}
	}
	n += len(s.ProjectsInTeam(w.team.Meta.ID))
	n += len(s.PolicyBindingsForProject(w.live.Meta.ID))
	for _, u := range w.userIDs {
		n += len(s.GroupsForUser(u)) + len(s.KeyHashesForUser(u))
		if v, ok := s.TokenVersion(u); ok {
			n += v
		}
	}
	n += len(s.Dependents(refPolicy, w.pols[0].Meta.ID))
	n += int(s.Generation())
	return n
}

// TestConcurrentWritersAgainstSnapshotReaders runs M writers over a
// random event stream against N readers walking every accessor. Under
// `-race` a clone that shares a slice or map with the published snapshot,
// or an Apply that mutates in place, shows up as a data race. After the
// writers quiesce the snapshot must still satisfy the reconciler's
// invariants and its forward/reverse ref maps must agree.
func TestConcurrentWritersAgainstSnapshotReaders(t *testing.T) {
	w := newRaceWorld(t)
	evs := w.events()

	const (
		writers        = 8
		readers        = 8
		eventsPerWrite = 200
	)
	stop := make(chan struct{})
	var writing, reading sync.WaitGroup

	for i := 0; i < readers; i++ {
		reading.Add(1)
		go func() {
			defer reading.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Hold the pointer across many reads: the whole point of COW
				// is that this stays consistent while writers publish.
				s := w.cat.Current()
				for j := 0; j < 4; j++ {
					_ = w.readAccessors(s)
				}
			}
		}()
	}
	for i := 0; i < writers; i++ {
		writing.Add(1)
		go func(seed int64) {
			defer writing.Done()
			r := rand.New(rand.NewSource(seed))
			for j := 0; j < eventsPerWrite; j++ {
				if err := evs[r.Intn(len(evs))](); err != nil {
					t.Errorf("writer %d event %d rejected: %v", seed, j, err)
				}
			}
		}(int64(i) + 1)
	}
	writing.Wait()
	close(stop)
	reading.Wait()

	assertRefsSymmetric(t, w.cat.Current(), "after the writers quiesced")

	// A full Reload from the same stores must converge on a snapshot with
	// the same ref-graph health as the incremental path landed on — a
	// reconcile that lost or leaked an edge under contention diverges here.
	must(t, w.cat.Reload(context.Background()))
	assertRefsSymmetric(t, w.cat.Current(), "after a final Reload")
}

// A snapshot a reader already holds stays as published whatever lands afterwards: every index, down to slice and set contents, renders the same after each of a run of incremental writes. A full reload builds from the stores and would cut the later snapshots loose from the held one, so the run leaves the two reload events out.
func TestPublishedSnapshotIsNeverMutatedByALaterApply(t *testing.T) {
	w := newRaceWorld(t)
	held := w.cat.Current()
	before := fmt.Sprintf("%v", *held)
	evs := w.events()
	evs = evs[:len(evs)-2]
	r := rand.New(rand.NewSource(7))
	for i := range 400 {
		if err := evs[r.Intn(len(evs))](); err != nil {
			t.Fatalf("event %d rejected: %v", i, err)
		}
		if got := fmt.Sprintf("%v", *held); got != before {
			t.Fatalf("event %d changed the snapshot a reader was holding", i)
		}
	}
	if w.cat.Current().Generation() <= held.Generation() {
		t.Fatal("400 writes published no newer snapshot")
	}
}
