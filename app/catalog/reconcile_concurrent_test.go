// reconcile_concurrent_test.go drives the COW reconciler from many goroutines at
// once. The reconciler's contract is that Apply*/Reload clone under rmu and
// publish with an atomic store, so a Current() holder keeps reading a
// consistent snapshot while writers churn. Only `-race` can observe a
// violation of that (a torn read is a benign-looking wrong answer without
// it), so these live behind `make test-race`.
package catalog

import (
	"context"
	"math/rand"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// raceVersionList hands back a fresh map per call: the snapshot keeps the
// map it is given, so a shared one would let a later Reload mutate what an
// older snapshot's readers are still walking.
type raceVersionList map[string]int

func (l raceVersionList) TokenVersions(context.Context) (map[string]int, error) {
	out := make(map[string]int, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out, nil
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
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	return &raceWorld{
		cat: c, models: models, keys: keys, pols: allPols, rks: rks,
		team: f.team, live: f.live, dead: f.disabled, role: f.role, grp: grp, sa: sa,
		rbs:     rbList{f.global, f.teamWide, f.inProject, f.onDead},
		pbs:     pbList{f.low, f.highA, f.noPol, f.onDeadP},
		userIDs: users,
	}
}

// events is the writer alphabet: every entry publishes a fresh copy of one
// row, so nothing a live snapshot points at is mutated after publication.
func (w *raceWorld) events() []func() {
	c := w.cat
	on := true
	var out []func()
	add := func(fn func()) { out = append(out, fn) }

	for i := range w.models {
		m := w.models[i]
		add(func() {
			cp := *m
			cp.Spec.Enabled = &on
			_ = c.ApplyModelUpsert(&cp)
		})
		add(func() { _ = c.ApplyModelDelete(m.Meta.ID) })
	}
	for i := range w.keys {
		k := w.keys[i]
		add(func() {
			cp := *k
			cp.Spec.Enabled = &on
			_ = c.ApplyHostKeyUpsert(&cp)
		})
		add(func() { _ = c.ApplyHostKeyDelete(k.Meta.ID) })
	}
	for i := range w.rks {
		rk := w.rks[i]
		add(func() {
			cp := *rk
			cp.Spec.Enabled = &on
			_ = c.ApplyKeyUpsert(&cp)
		})
		add(func() { _ = c.ApplyKeyDelete(rk.Meta.ID) })
	}
	for i := range w.pols {
		p := w.pols[i]
		add(func() {
			cp := *p
			cp.Spec.Enabled = &on
			_ = c.ApplyPolicyUpsert(&cp)
		})
	}

	// Tenancy churn: disable/enable a project, re-upsert the team, and
	// delete/revive the service account whose keys hang off it.
	add(func() {
		cp := *w.live
		cp.Spec.Enabled = &on
		_ = c.ApplyProjectUpsert(&cp)
	})
	add(func() {
		cp := *w.dead
		cp.Spec.Enabled = &on
		_ = c.ApplyProjectUpsert(&cp)
	})
	add(func() { _ = c.ApplyProjectDelete(w.dead.Meta.ID) })
	add(func() {
		cp := *w.team
		cp.Spec.Enabled = &on
		_ = c.ApplyTeamUpsert(&cp)
	})
	add(func() {
		cp := *w.sa
		cp.Spec.Enabled = &on
		_ = c.ApplyServiceAccountUpsert(&cp)
	})
	add(func() { _ = c.ApplyServiceAccountDelete(w.sa.Meta.ID) })

	// Subject churn: group membership and binding subject lists are what
	// the subject index is rebuilt from.
	for n := 0; n <= len(w.userIDs); n++ {
		members := append([]string(nil), w.userIDs[:n]...)
		add(func() {
			cp := *w.grp
			cp.Spec.MemberIDs = members
			cp.Spec.Enabled = &on
			_ = c.ApplyGroupUpsert(&cp)
		})
	}
	add(func() { _ = c.ApplyGroupDelete(w.grp.Meta.ID) })
	for i := range w.rbs {
		b := w.rbs[i]
		add(func() {
			cp := *b
			cp.Spec.Subjects = []rolebinding.Subject{
				{Kind: rolebinding.SubjectGroup, Name: w.grp.Meta.Name},
			}
			_ = c.ApplyRoleBindingUpsert(&cp)
		})
		add(func() { _ = c.ApplyRoleBindingDelete(b.Meta.ID) })
	}
	for i := range w.pbs {
		b := w.pbs[i]
		add(func() {
			cp := *b
			cp.Spec.Subjects = []rolebinding.Subject{
				{Kind: rolebinding.SubjectGroup, Name: w.grp.Meta.Name},
			}
			_ = c.ApplyPolicyBindingUpsert(&cp)
		})
		add(func() { _ = c.ApplyPolicyBindingDelete(b.Meta.ID) })
	}
	add(func() {
		cp := *w.role
		cp.Spec.Enabled = &on
		_ = c.ApplyRoleUpsert(&cp)
	})

	// Whole-snapshot republishes racing the incremental ones.
	add(func() { _ = c.Reload(context.Background()) })
	add(func() { _ = c.ReloadTokenVersions(context.Background()) })

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
		n += len(s.ModelsInPolicy(p.Meta.ID)) + len(s.HostKeysInPolicy(p.Meta.ID))
		if s.RateLimitOfPolicy(p.Meta.ID) != nil {
			n++
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
				evs[r.Intn(len(evs))]()
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
	if err := w.cat.Reload(context.Background()); err != nil {
		t.Fatalf("final reload: %v", err)
	}
	assertRefsSymmetric(t, w.cat.Current(), "after a final Reload")
}

// assertRefsSymmetric checks the ref graph in both directions using the
// production presence test (the refs_test.go helper predates the tenancy
// kinds and reads them as absent): every present row's outbound parent
// lists it back, and every recorded dependent is itself present.
func assertRefsSymmetric(t *testing.T, s *Snapshot, when string) {
	t.Helper()
	forward := func(child refKey, parents []refKey) {
		for _, p := range parents {
			if !rowPresent(s, p) {
				t.Errorf("%s: %s/%s references a missing %s/%s",
					when, child.Kind, child.ID, p.Kind, p.ID)
				continue
			}
			if !containsRef(s.Dependents(p.Kind, p.ID), child) {
				t.Errorf("%s: refsBy%s[%s] is missing dependent %s/%s",
					when, p.Kind, p.ID, child.Kind, child.ID)
			}
		}
	}
	for _, m := range s.modelsByID {
		forward(refKey{Kind: refModel, ID: m.Meta.ID}, outboundModelRefs(m))
	}
	for _, k := range s.hostKeysByID {
		forward(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	}
	for _, p := range s.policiesByID {
		forward(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	}
	for _, k := range s.keysByID {
		forward(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundKeyRefs(k))
	}
	for _, sa := range s.serviceAccountsByID {
		forward(refKey{Kind: refServiceAccount, ID: sa.Meta.ID}, outboundServiceAccountRefs(sa))
	}
	for _, b := range s.roleBindingsByID {
		forward(refKey{Kind: refRoleBinding, ID: b.Meta.ID}, outboundRoleBindingRefs(b))
	}
	for _, b := range s.policyBindingsByID {
		forward(refKey{Kind: refPolicyBinding, ID: b.Meta.ID}, outboundPolicyBindingRefs(b))
	}
	for _, p := range s.projectsByID {
		forward(refKey{Kind: refProject, ID: p.Meta.ID}, outboundProjectRefs(p))
	}

	// The dual: nothing recorded as a dependent may have been dropped.
	for name, m := range map[string]map[string]refSet{
		"Provider": s.refsByProvider, "Host": s.refsByHost, "Model": s.refsByModel,
		"HostKey": s.refsByHostKey, "RateLimit": s.refsByRateLimit, "Policy": s.refsByPolicy,
		"Team": s.refsByTeam, "Project": s.refsByProject,
		"ServiceAccount": s.refsByServiceAccount, "Role": s.refsByRole,
	} {
		for parentID, set := range m {
			for child := range set {
				if !rowPresent(s, child) {
					t.Errorf("%s: refsBy%s[%s] holds a dropped dependent %s/%s",
						when, name, parentID, child.Kind, child.ID)
				}
			}
		}
	}
}

func containsRef(set []refKey, want refKey) bool {
	for _, k := range set {
		if k == want {
			return true
		}
	}
	return false
}

// TestPublishedSnapshotIsNeverMutatedByALaterApply pins the other half
// of COW: a snapshot a reader already holds must be byte-stable no matter
// what lands afterwards. Without -race an in-place index write is invisible
// here unless it happens to change a length, so the fingerprint compares
// contents, not counts.
func TestPublishedSnapshotIsNeverMutatedByALaterApply(t *testing.T) {
	w := newRaceWorld(t)
	held := w.cat.Current()
	before := fingerprint(held)

	evs := w.events()
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 400; i++ {
		evs[r.Intn(len(evs))]()
	}
	if got := fingerprint(held); got != before {
		t.Fatalf("the snapshot a reader was holding changed under it:\n before %s\n after  %s", before, got)
	}
}

// fingerprint renders the identity-bearing contents of a snapshot's indices
// in a stable order.
func fingerprint(s *Snapshot) string {
	var ids []string
	collect := func(prefix string, in []string) {
		sorted := append([]string(nil), in...)
		sortStrings(sorted)
		for _, v := range sorted {
			ids = append(ids, prefix+"/"+v)
		}
	}
	collect("model", idsOf(s.modelsByID, func(m *model.Model) string { return m.Meta.ID }))
	collect("policy", idsOf(s.policiesByID, func(p *policy.Policy) string { return p.Meta.ID }))
	collect("key", idsOf(s.keysByID, func(k *key.Key) string { return k.Meta.ID }))
	collect("project", idsOf(s.projectsByID, func(p *project.Project) string { return p.Meta.ID }))
	collect("group", idsOf(s.groupsByID, func(g *group.Group) string { return g.Meta.ID }))
	collect("sa", idsOf(s.serviceAccountsByID, func(a *serviceaccount.ServiceAccount) string { return a.Meta.ID }))
	collect("rb", idsOf(s.roleBindingsByID, func(b *rolebinding.RoleBinding) string { return b.Meta.ID }))
	collect("pb", idsOf(s.policyBindingsByID, func(b *policybinding.PolicyBinding) string { return b.Meta.ID }))
	out := ""
	for _, v := range ids {
		out += v + " "
	}
	return out
}

func idsOf[T any](m map[string]T, id func(T) string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, id(v))
	}
	return out
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
