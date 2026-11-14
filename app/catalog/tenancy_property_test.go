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
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
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
	// A personal key holds no policy, so it lives independently of the
	// project and its policy.
	userID := meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
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
	must(t, cat.Reload(context.Background()))
	return cat
}

func enabledFlag(p *bool) bool { return p == nil || *p }

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

	all := propertyActions(t, c, cat)

	for i := 0; i < events; i++ {
		all[r.Intn(len(all))]()
		assertRefsSymmetric(t, cat.Current(), fmt.Sprint("step ", i))
		assertChainConsistency(t, cat.Current(), c, i)
		if t.Failed() {
			return
		}
	}
	assertSameMembership(t, cat.Current(), c.catalog(t).Current())
}

// propertyActions are the events the property draws from. Each mutates the row slices the way a control-plane write would mutate Postgres, then applies the matching incremental update, which must be accepted: every row written is valid. A toggle or a revive is an upsert, so it also puts a deleted row back in the store.
func propertyActions(t *testing.T, c *tenancyChain, cat *Catalog) []func() {
	on := true
	var out []func()
	add := func(enabled **bool, restore func(), upsert func() error, remove func(), del func(string) error, id string) {
		out = append(out,
			func() { *enabled = flip(*enabled); restore(); must(t, upsert()) },
			func() { *enabled = &on; restore(); must(t, upsert()) },
			func() { remove(); must(t, del(id)) })
	}
	add(&c.team.Spec.Enabled, func() { c.teams = []*team.Team{c.team} },
		func() error { cp := *c.team; return cat.ApplyTeamUpsert(&cp) }, func() { c.teams = nil }, cat.ApplyTeamDelete, c.team.Meta.ID)
	add(&c.project.Spec.Enabled, func() { c.projects = []*project.Project{c.project} },
		func() error { cp := *c.project; return cat.ApplyProjectUpsert(&cp) }, func() { c.projects = nil }, cat.ApplyProjectDelete, c.project.Meta.ID)
	add(&c.policy.Spec.Enabled, func() { c.policies = []*policy.Policy{c.policy} },
		func() error { cp := *c.policy; return cat.ApplyPolicyUpsert(&cp) }, func() { c.policies = nil }, cat.ApplyPolicyDelete, c.policy.Meta.ID)
	add(&c.key.Spec.Enabled, func() { c.keys = []*key.Key{c.key} },
		func() error { cp := *c.key; return cat.ApplyKeyUpsert(&cp) }, func() { c.keys = nil }, cat.ApplyKeyDelete, c.key.Meta.ID)
	return out
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
	wantKey := len(c.keys) > 0 && enabledFlag(c.key.Spec.Enabled)
	if got, _ := s.KeyByHash(c.key.Spec.KeyHash); (got != nil) != wantKey {
		t.Fatalf("step %d: key present = %v, want %v", step, got != nil, wantKey)
	}
	if !projectPresent {
		if _, ok := s.Policy(c.policy.Meta.ID); ok {
			t.Fatalf("step %d: a project-owned policy outlived its project", step)
		}
		if _, ok := s.DisabledPolicy(c.policy.Meta.ID); ok {
			t.Fatalf("step %d: the retained disabled copy of a project-owned policy outlived its project", step)
		}
		if !teamPresent && len(s.ProjectsInTeam(c.team.Meta.ID)) != 0 {
			t.Fatalf("step %d: a team that is gone still lists projects", step)
		}
		return
	}
	// With the project present the policy is held while it resolves; a
	// disabled one is kept aside rather than dropped.
	policyResolves := len(c.policies) > 0
	if _, live := s.Policy(c.policy.Meta.ID); live != (policyResolves && enabledFlag(c.policy.Spec.Enabled)) {
		t.Fatalf("step %d: policy in the routing index = %v, want %v", step, live, policyResolves && enabledFlag(c.policy.Spec.Enabled))
	}
	if _, ok := s.DisabledPolicy(c.policy.Meta.ID); ok != (policyResolves && !enabledFlag(c.policy.Spec.Enabled)) {
		t.Fatalf("step %d: policy retained as disabled = %v, want %v", step, ok, policyResolves && !enabledFlag(c.policy.Spec.Enabled))
	}
}

// assertSameMembership is the build == reconcile fixpoint: both snapshots hold the same rows of every kind.
func assertSameMembership(t *testing.T, reconciled, built *Snapshot) {
	t.Helper()
	if got, want := membership(reconciled), membership(built); !slices.Equal(got, want) {
		t.Errorf("reconciled snapshot holds\n%v\na build from the same rows holds\n%v", got, want)
	}
}

// membership lists the rows a snapshot holds as sorted "kind/id" entries.
func membership(s *Snapshot) []string {
	var out []string
	add := func(kind string, ids ...string) {
		for _, id := range ids {
			out = append(out, kind+"/"+id)
		}
	}
	add("provider", slices.Collect(maps.Keys(s.providersByID))...)
	add("host", slices.Collect(maps.Keys(s.hostsByID))...)
	add("model", slices.Collect(maps.Keys(s.modelsByID))...)
	add("hostkey", slices.Collect(maps.Keys(s.hostKeysByID))...)
	add("ratelimit", slices.Collect(maps.Keys(s.rateLimitsByID))...)
	add("policy", slices.Collect(maps.Keys(s.policiesByID))...)
	add("disabled-policy", slices.Collect(maps.Keys(s.disabledPoliciesByID))...)
	add("pricing", slices.Collect(maps.Keys(s.pricingsByID))...)
	add("binding", slices.Collect(maps.Keys(s.bindingsByID))...)
	add("key", slices.Collect(maps.Keys(s.keysByID))...)
	add("key-hash", slices.Collect(maps.Keys(s.keysByHash))...)
	add("team", slices.Collect(maps.Keys(s.teamsByID))...)
	add("project", slices.Collect(maps.Keys(s.projectsByID))...)
	add("serviceaccount", slices.Collect(maps.Keys(s.serviceAccountsByID))...)
	add("group", slices.Collect(maps.Keys(s.groupsByID))...)
	add("role", slices.Collect(maps.Keys(s.rolesByID))...)
	add("rolebinding", slices.Collect(maps.Keys(s.roleBindingsByID))...)
	add("policybinding", slices.Collect(maps.Keys(s.policyBindingsByID))...)
	slices.Sort(out)
	return out
}
