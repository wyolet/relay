package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// disabledPolicyFixture is one project holding a policy an operator switched
// off, plus each row kind that names it.
type disabledPolicyFixture struct {
	team    *team.Team
	project *project.Project
	pol     *policy.Policy
	sa      *serviceaccount.ServiceAccount
	key     *key.Key
	binding *policybinding.PolicyBinding
}

func newDisabledPolicyFixture(enabled bool) disabledPolicyFixture {
	f := disabledPolicyFixture{}
	f.team = &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	f.project = &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml"},
		Spec: project.Spec{TeamID: f.team.Meta.ID},
	}
	f.project.StampOwner()
	f.pol = &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-default", Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: policy.Spec{Enabled: &enabled},
	}
	f.sa = &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "svc"},
		Spec: serviceaccount.Spec{ProjectID: f.project.Meta.ID, PolicyID: f.pol.Meta.ID},
	}
	f.sa.StampOwner()
	f.key = &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "k", Owner: meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: f.sa.Meta.ID},
			PolicyID:  f.pol.Meta.ID, KeyHash: "hash-k",
		},
	}
	prio := 10
	f.binding = &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb"},
		Spec: policybinding.Spec{
			ProjectID: f.project.Meta.ID, PolicyID: f.pol.Meta.ID, Priority: &prio,
			Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}},
		},
	}
	f.binding.StampOwner()
	return f
}

func (f disabledPolicyFixture) catalog(t *testing.T) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{},
		rkList{f.key}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.project}, saList{f.sa}, grpList{},
		roleList{}, rbList{}, pbList{f.binding})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

// D77: a key, service account, and policy binding that name a
// disabled policy all stay in the snapshot; the policy itself is out of every
// routing index but resolvable, so a request answers policy_disabled rather
// than falling through to a broader grant or a 401.
func TestDisabledPolicyKeepsDependentRows(t *testing.T) {
	f := newDisabledPolicyFixture(false)
	s := f.catalog(t).Current()

	if _, ok := s.Policy(f.pol.Meta.ID); ok {
		t.Error("a disabled policy is still in the routing index")
	}
	if _, ok := s.DisabledPolicy(f.pol.Meta.ID); !ok {
		t.Fatal("the disabled policy is not resolvable — its dependents cannot answer policy_disabled")
	}
	if k, _ := s.KeyByHash("hash-k"); k == nil {
		t.Error("the key was dropped; its request would answer 401 invalid key")
	}
	if _, ok := s.ServiceAccount(f.sa.Meta.ID); !ok {
		t.Error("the service account was dropped")
	}
	if got := s.PolicyBindingsForProject(f.project.Meta.ID); len(got) != 1 {
		t.Errorf("policy bindings = %d, want the one naming the disabled policy kept", len(got))
	}
}

// D77: disabling through the NOTIFY path reaches the same
// state as a full reload — the incremental cascade used to evict the rows.
func TestDisablePolicyViaReconcileKeepsDependentRows(t *testing.T) {
	f := newDisabledPolicyFixture(true)
	c := f.catalog(t)
	off := false
	turnedOff := *f.pol
	turnedOff.Spec.Enabled = &off
	if err := c.ApplyPolicyUpsert(&turnedOff); err != nil {
		t.Fatalf("disable policy: %v", err)
	}
	s := c.Current()
	if _, ok := s.Policy(f.pol.Meta.ID); ok {
		t.Error("a disabled policy is still in the routing index")
	}
	if _, ok := s.DisabledPolicy(f.pol.Meta.ID); !ok {
		t.Fatal("the disabled policy is not resolvable after the reconcile")
	}
	if k, _ := s.KeyByHash("hash-k"); k == nil {
		t.Error("disabling the policy evicted its key")
	}
	if got := s.PolicyBindingsForProject(f.project.Meta.ID); len(got) != 1 {
		t.Errorf("policy bindings = %d, want the binding kept", len(got))
	}

	// Re-enabling puts it back in the routing index.
	if err := c.ApplyPolicyUpsert(f.pol); err != nil {
		t.Fatalf("re-enable policy: %v", err)
	}
	if _, ok := c.Current().Policy(f.pol.Meta.ID); !ok {
		t.Error("re-enabling did not restore the policy")
	}
	if _, ok := c.Current().DisabledPolicy(f.pol.Meta.ID); ok {
		t.Error("re-enabling left the policy marked disabled")
	}
}

// A disabled policy is present, so re-enabling it is an incremental patch,
// not an absent-id recovery: it must not drag the whole catalog through a
// reload from the stores.
func TestReEnablingADisabledPolicyDoesNotFullReload(t *testing.T) {
	f := newDisabledPolicyFixture(false)
	c := f.catalog(t)
	before := c.Current().Generation()

	on := true
	turnedOn := *f.pol
	turnedOn.Spec.Enabled = &on
	if err := c.ApplyPolicyUpsert(&turnedOn); err != nil {
		t.Fatalf("re-enable policy: %v", err)
	}
	if _, ok := c.Current().Policy(f.pol.Meta.ID); !ok {
		t.Fatal("re-enabling did not restore the policy")
	}
	// A recovery reload rebuilds from every store and burns extra
	// generations; the incremental path publishes exactly one new snapshot.
	if got := c.Current().Generation(); got != before+1 {
		t.Errorf("generation went %d → %d, want a single incremental publish", before, got)
	}
}

// A disabled policy still holds its outbound refs, so losing its project
// evicts it. Otherwise it lingers with a dangling owner and the rows naming
// it keep answering 403 against a policy no operator can reach any more.
func TestDeletingTheProjectEvictsItsDisabledPolicy(t *testing.T) {
	f := newDisabledPolicyFixture(false)
	c := f.catalog(t)
	if _, ok := c.Current().DisabledPolicy(f.pol.Meta.ID); !ok {
		t.Fatal("the disabled policy is missing before the project delete")
	}
	if err := c.ApplyProjectDelete(f.project.Meta.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	s := c.Current()
	if _, ok := s.DisabledPolicy(f.pol.Meta.ID); ok {
		t.Error("the disabled policy outlived its project")
	}
	if k, _ := s.KeyByHash("hash-k"); k != nil {
		t.Error("the key outlived the policy it names")
	}
}
