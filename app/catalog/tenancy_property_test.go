package catalog

// tenancy_property_test.go fuzzes the chain a customer credential hangs off:
// key → project-owned policy → project → team. Every link is a hard ref, so
// removing any of them has to evict everything below it — and the incremental
// reconciler has to reach the same snapshot a fresh build from the surviving
// rows would produce. A drift here is a key that keeps authenticating against
// a project the operator removed.

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// mutableRows is a store whose contents follow the events the property
// applies. The reconciler falls back to a full rebuild whenever an event
// names a row it does not hold, and that rebuild re-reads the stores — so a
// fixed lister would resurrect rows the property just deleted and the
// build-equals-reconcile check would compare against a fiction.
type mutableRows[T any] struct{ rows *[]*T }

func (m mutableRows[T]) List(context.Context) ([]*T, error) { return *m.rows, nil }

// tenancyChain is the row set the property operates on: one team, one project
// in it, that project's policy, and a key that names the policy.
type tenancyChain struct {
	team    *team.Team
	project *project.Project
	policy  *policy.Policy
	key     *key.Key

	teams    []*team.Team
	projects []*project.Project
	policies []*policy.Policy
	keys     []*key.Key
}

func newTenancyChain() *tenancyChain {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
	}
	// A personal key pointed at a project's policy: it spends that project's
	// credentials, so it has to disappear with the project even though the
	// row itself is owned by a user.
	userID := meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
			PolicyID:  pol.Meta.ID,
			KeyHash:   strings.Repeat("c", 64),
		},
	}
	return &tenancyChain{
		team: tm, project: proj, policy: pol, key: k,
		teams: []*team.Team{tm}, projects: []*project.Project{proj},
		policies: []*policy.Policy{pol}, keys: []*key.Key{k},
	}
}

// catalog wires a Catalog over the chain's mutable row slices.
func (c *tenancyChain) catalog(t *testing.T) *Catalog {
	t.Helper()
	cat := New(
		mutableRows[provider.Provider]{&[]*provider.Provider{}},
		mutableRows[host.Host]{&[]*host.Host{}},
		mutableRows[policy.Policy]{&c.policies},
		mutableRows[model.Model]{&[]*model.Model{}},
		mutableRows[hostkey.HostKey]{&[]*hostkey.HostKey{}},
		mutableRows[ratelimit.RateLimit]{&[]*ratelimit.RateLimit{}},
		mutableRows[key.Key]{&c.keys},
		mutableRows[pricing.Pricing]{&[]*pricing.Pricing{}},
		mutableRows[binding.Binding]{&[]*binding.Binding{}},
	)
	cat.UseTenancy(
		mutableRows[team.Team]{&c.teams},
		mutableRows[project.Project]{&c.projects},
		mutableRows[serviceaccount.ServiceAccount]{&[]*serviceaccount.ServiceAccount{}},
		mutableRows[group.Group]{&[]*group.Group{}},
		mutableRows[role.Role]{&[]*role.Role{}},
		mutableRows[rolebinding.RoleBinding]{&[]*rolebinding.RoleBinding{}},
		mutableRows[policybinding.PolicyBinding]{&[]*policybinding.PolicyBinding{}},
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return cat
}

func flip(p *bool) *bool {
	cur := true
	if p != nil {
		cur = *p
	}
	next := !cur
	return &next
}

func on() *bool { v := true; return &v }

func enabledFlag(p *bool) bool { return p == nil || *p }

// mustApply panics on a reconcile error. Every row this property writes is a
// valid one, so a rejected write is a broken fixture — and a swallowed one
// would look like a snapshot that lost a row.
func mustApply(err error) {
	if err != nil {
		panic("catalog: property event rejected: " + err.Error())
	}
}

// TestProperty_CredentialChainSurvivesTenancyChurn applies a random sequence
// of tenancy, policy and key events and checks after each one that the
// snapshot holds no dangling reference, that a credential is reachable
// exactly while every link above it is, and that the reconciled snapshot
// matches a build from the same surviving rows.
func TestProperty_CredentialChainSurvivesTenancyChurn(t *testing.T) {
	for _, seed := range []int64{1, 5, 42, 777, 31337} {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) { runTenancyProperty(t, seed, 300) })
	}
}

func runTenancyProperty(t *testing.T, seed int64, events int) {
	r := rand.New(rand.NewSource(seed))
	c := newTenancyChain()
	cat := c.catalog(t)

	all := propertyActions(c, cat)

	for i := 0; i < events; i++ {
		all[r.Intn(len(all))]()
		assertSnapshotInvariants(t, cat.Current(), i)
		assertChainConsistency(t, cat.Current(), c, i)
		if t.Failed() {
			return
		}
	}
	assertSameMembership(t, cat.Current(), c.catalog(t).Current(), c)
}

// propertyActions are the events the property draws from. Each mutates the
// row slices the same way a control-plane write would mutate Postgres, then
// applies the matching incremental update.
func propertyActions(c *tenancyChain, cat *Catalog) []func() {
	return []func(){
		// A toggle is an upsert, so it also puts the row back in the store —
		// exactly what a control-plane PUT of a previously deleted row does.
		func() {
			c.team.Spec.Enabled = flip(c.team.Spec.Enabled)
			c.teams = []*team.Team{c.team}
			cp := *c.team
			mustApply(cat.ApplyTeamUpsert(&cp))
		},
		func() {
			c.project.Spec.Enabled = flip(c.project.Spec.Enabled)
			c.projects = []*project.Project{c.project}
			cp := *c.project
			mustApply(cat.ApplyProjectUpsert(&cp))
		},
		func() {
			c.policy.Spec.Enabled = flip(c.policy.Spec.Enabled)
			c.policies = []*policy.Policy{c.policy}
			cp := *c.policy
			mustApply(cat.ApplyPolicyUpsert(&cp))
		},
		func() {
			c.key.Spec.Enabled = flip(c.key.Spec.Enabled)
			c.keys = []*key.Key{c.key}
			cp := *c.key
			mustApply(cat.ApplyKeyUpsert(&cp))
		},
		func() { c.teams = nil; mustApply(cat.ApplyTeamDelete(c.team.Meta.ID)) },
		func() { c.projects = nil; mustApply(cat.ApplyProjectDelete(c.project.Meta.ID)) },
		func() { c.policies = nil; mustApply(cat.ApplyPolicyDelete(c.policy.Meta.ID)) },
		func() { c.keys = nil; mustApply(cat.ApplyKeyDelete(c.key.Meta.ID)) },
		func() {
			c.team.Spec.Enabled = on()
			c.teams = []*team.Team{c.team}
			cp := *c.team
			mustApply(cat.ApplyTeamUpsert(&cp))
		},
		func() {
			c.project.Spec.Enabled = on()
			c.projects = []*project.Project{c.project}
			cp := *c.project
			mustApply(cat.ApplyProjectUpsert(&cp))
		},
		func() {
			c.policy.Spec.Enabled = on()
			c.policies = []*policy.Policy{c.policy}
			cp := *c.policy
			mustApply(cat.ApplyPolicyUpsert(&cp))
		},
		func() {
			c.key.Spec.Enabled = on()
			c.keys = []*key.Key{c.key}
			cp := *c.key
			mustApply(cat.ApplyKeyUpsert(&cp))
		},
	}
}

// assertChainConsistency checks the rule the chain exists to express: a
// credential is reachable only while every link above it is.
func assertChainConsistency(t *testing.T, s *Snapshot, c *tenancyChain, step int) {
	t.Helper()
	teamPresent := len(c.teams) > 0 && enabledFlag(c.team.Spec.Enabled)
	projectPresent := teamPresent && len(c.projects) > 0 && enabledFlag(c.project.Spec.Enabled)

	if _, ok := s.Team(c.team.Meta.ID); ok != teamPresent {
		t.Fatalf("step %d: team present = %v, want %v", step, ok, teamPresent)
	}
	if _, ok := s.Project(c.project.Meta.ID); ok != projectPresent {
		t.Fatalf("step %d: project present = %v, want %v (team present %v)", step, ok, projectPresent, teamPresent)
	}
	if !projectPresent {
		if _, ok := s.Policy(c.policy.Meta.ID); ok {
			t.Fatalf("step %d: a project-owned policy outlived its project", step)
		}
		if _, ok := s.DisabledPolicy(c.policy.Meta.ID); ok {
			t.Fatalf("step %d: the retained disabled copy of a project-owned policy outlived its project", step)
		}
		if got, _ := s.KeyByHash(c.key.Spec.KeyHash); got != nil {
			t.Fatalf("step %d: a key still authenticates against a project that is gone", step)
		}
		if !teamPresent && len(s.ProjectsInTeam(c.team.Meta.ID)) != 0 {
			t.Fatalf("step %d: a team that is gone still lists projects", step)
		}
		return
	}
	// With the project present the key survives exactly while its policy
	// resolves — disabled included, since a disabled policy answers 403
	// rather than turning a valid key into an unknown one.
	policyResolves := len(c.policies) > 0
	if _, live := s.Policy(c.policy.Meta.ID); live != (policyResolves && enabledFlag(c.policy.Spec.Enabled)) {
		t.Fatalf("step %d: policy in the routing index = %v, want %v", step, live, policyResolves && enabledFlag(c.policy.Spec.Enabled))
	}
	if _, ok := s.DisabledPolicy(c.policy.Meta.ID); ok != (policyResolves && !enabledFlag(c.policy.Spec.Enabled)) {
		t.Fatalf("step %d: policy retained as disabled = %v, want %v", step, ok, policyResolves && !enabledFlag(c.policy.Spec.Enabled))
	}
	wantKey := len(c.keys) > 0 && enabledFlag(c.key.Spec.Enabled) && policyResolves
	got, _ := s.KeyByHash(c.key.Spec.KeyHash)
	if (got != nil) != wantKey {
		t.Fatalf("step %d: key present = %v, want %v (policy resolves %v, key enabled %v)",
			step, got != nil, wantKey, policyResolves, enabledFlag(c.key.Spec.Enabled))
	}
}

// assertSameMembership is the build == reconcile fixpoint over the rows this
// property touches.
func assertSameMembership(t *testing.T, reconciled, built *Snapshot, c *tenancyChain) {
	t.Helper()
	_, rTeam := reconciled.Team(c.team.Meta.ID)
	_, bTeam := built.Team(c.team.Meta.ID)
	if rTeam != bTeam {
		t.Errorf("team: reconcile %v, build %v", rTeam, bTeam)
	}
	_, rProj := reconciled.Project(c.project.Meta.ID)
	_, bProj := built.Project(c.project.Meta.ID)
	if rProj != bProj {
		t.Errorf("project: reconcile %v, build %v", rProj, bProj)
	}
	_, rPol := reconciled.Policy(c.policy.Meta.ID)
	_, bPol := built.Policy(c.policy.Meta.ID)
	if rPol != bPol {
		t.Errorf("policy: reconcile %v, build %v", rPol, bPol)
	}
	_, rDis := reconciled.DisabledPolicy(c.policy.Meta.ID)
	_, bDis := built.DisabledPolicy(c.policy.Meta.ID)
	if rDis != bDis {
		t.Errorf("disabled policy: reconcile %v, build %v", rDis, bDis)
	}
	rKey, _ := reconciled.KeyByHash(c.key.Spec.KeyHash)
	bKey, _ := built.KeyByHash(c.key.Spec.KeyHash)
	if (rKey != nil) != (bKey != nil) {
		t.Errorf("key: reconcile %v, build %v", rKey != nil, bKey != nil)
	}
}
