//go:build integration

// policy_binding_guards_integration_test.go covers the two write paths that
// hand a caller the right to spend a policy: creating a PolicyBinding, and
// attaching a Key to a policy from the policy side. Both read through the
// concrete stores, which have no fake seam.
// Run with: make test-integration.
package control

import (
	"context"
	"strings"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// bindingFixture is the row graph the binding guards read: two projects in one
// team, each with a policy, plus a host with a tier policy and a shared one.
type bindingFixture struct {
	deps    Deps
	stores  *appcatalog.Stores
	mine    *project.Project
	theirs  *project.Project
	ownPol  *policy.Policy
	farPol  *policy.Policy
	shared  *policy.Policy
	tier    *policy.Policy
	account *serviceaccount.ServiceAccount
}

func newBindingFixture(t *testing.T) (bindingFixture, context.Context) {
	t.Helper()
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	ctx = visibleCtx(ctx)
	w := bindingFixture{deps: Deps{Authz: testRBAC(), Stores: stores}, stores: stores}

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "bind-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	mkProject := func(name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: name}, Spec: project.Spec{TeamID: tm.Meta.ID}}
		p.StampOwner()
		if err := stores.Project.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert project %s: %v", name, err)
		}
		return p
	}
	w.mine, w.theirs = mkProject("bind-mine"), mkProject("bind-theirs")

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "bind-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	mkPolicy := func(name string, owner meta.Owner) *policy.Policy {
		p := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}}
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert policy %s: %v", name, err)
		}
		return p
	}
	w.ownPol = mkPolicy("bind-own", meta.Owner{Kind: meta.OwnerProject, ID: w.mine.Meta.ID})
	w.farPol = mkPolicy("bind-far", meta.Owner{Kind: meta.OwnerProject, ID: w.theirs.Meta.ID})
	w.shared = mkPolicy("bind-shared", meta.Owner{Kind: meta.OwnerSystem})
	w.tier = mkPolicy("bind-tier", meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID})

	w.account = &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "bind-account"},
		Spec: serviceaccount.Spec{ProjectID: w.mine.Meta.ID},
	}
	w.account.StampOwner()
	if err := stores.ServiceAccount.Upsert(ctx, w.account); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}
	return w, ctx
}

// TestGuardPolicyBinding_CrossProject: a binding may hand out only a policy
// its own project owns or a system-owned shared one. Another project's policy
// and a host's tier policy are both refused — the first would let a project
// spend a credential that is not its own, the second has no inbound keys at
// all (D74).
func TestGuardPolicyBinding_CrossProject(t *testing.T) {
	w, ctx := newBindingFixture(t)
	guard := guardPolicyBinding(w.deps)

	mkBinding := func(projectID, policyID string) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "b-" + policyID[:8]},
			Spec: policybinding.Spec{
				ProjectID: projectID, PolicyID: policyID,
				Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectServiceAccount, ID: w.account.Meta.ID, Name: w.account.Meta.Name}},
			},
		}
	}

	for _, tc := range []struct {
		name     string
		policyID string
		wantErr  string // empty means accepted
	}{
		{name: "its own project's policy", policyID: w.ownPol.Meta.ID},
		{name: "a system-owned shared policy", policyID: w.shared.Meta.ID},
		{name: "another project's policy", policyID: w.farPol.Meta.ID, wantErr: w.farPol.Meta.Name},
		{name: "a host tier policy", policyID: w.tier.Meta.ID, wantErr: w.tier.Meta.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mkBinding(w.mine.Meta.ID, tc.policyID)
			err := guard(ctx, "create", nil, b)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("guard refused a bindable policy: %v", err)
				}
				// The guard re-derives the owner from the project and fills
				// the default priority, so the row reads back ordering by the
				// value it was stored with.
				if b.Meta.Owner.Kind != meta.OwnerProject || b.Meta.Owner.ID != w.mine.Meta.ID {
					t.Errorf("owner = %+v, want the binding's own project", b.Meta.Owner)
				}
				if b.Spec.Priority == nil || *b.Spec.Priority != policybinding.DefaultPriority {
					t.Errorf("priority = %v, want the default stamped in", b.Spec.Priority)
				}
				return
			}
			if err == nil {
				t.Fatalf("guard accepted a policy the project may not bind")
			}
			if got := statusOf(t, err); got != 400 && got != 404 {
				t.Fatalf("status = %d, want the reference refused", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}

	t.Run("an explicit priority is kept", func(t *testing.T) {
		zero := 0
		b := mkBinding(w.mine.Meta.ID, w.ownPol.Meta.ID)
		b.Spec.Priority = &zero
		if err := guard(ctx, "create", nil, b); err != nil {
			t.Fatalf("guard: %v", err)
		}
		if b.Spec.Priority == nil || *b.Spec.Priority != 0 {
			t.Fatalf("priority = %v, want the explicit zero kept", b.Spec.Priority)
		}
	})

	t.Run("a project that does not exist", func(t *testing.T) {
		b := mkBinding(meta.NewID(), w.shared.Meta.ID)
		if err := guard(ctx, "create", nil, b); err == nil {
			t.Fatal("guard accepted a binding into a project that does not exist")
		}
	})

	t.Run("a subject that does not exist", func(t *testing.T) {
		b := mkBinding(w.mine.Meta.ID, w.ownPol.Meta.ID)
		b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectServiceAccount, ID: meta.NewID(), Name: "no-such-account"}}
		if err := guard(ctx, "create", nil, b); err == nil {
			t.Fatal("guard accepted a binding naming a subject that does not exist")
		}
	})

	t.Run("a delete is not re-guarded", func(t *testing.T) {
		if err := guard(ctx, "delete", nil, nil); err != nil {
			t.Fatalf("delete guard: %v", err)
		}
	})
}

// TestPolicyKeysAttach_Guards: attaching a Key to a policy from the policy
// side goes through the same reference guard, row validation and hand-edit
// marking a PUT on the key does. That a tier policy is refused and a
// system-owned one accepted is pinned by
// TestWriteKeyPolicy_RunsTheGuardAndMarksDirty; what is left is the
// project-to-project rule, the detach, and a row that fails its own Validate.
func TestPolicyKeysAttach_Guards(t *testing.T) {
	w, ctx := newBindingFixture(t)

	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-guard-key", Owner: meta.Owner{Kind: meta.OwnerProject, ID: w.mine.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: w.account.Meta.ID},
			KeyHash:   strings.Repeat("b", 64), Prefix: "sk-guard",
		},
	}
	if err := w.stores.Key.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert key: %v", err)
	}

	for _, tc := range []struct {
		name     string
		policyID string
		accept   bool
	}{
		{name: "its own project's policy", policyID: w.ownPol.Meta.ID, accept: true},
		{name: "another project's policy", policyID: w.farPol.Meta.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k.Spec.PolicyID = tc.policyID
			k.Meta.Dirty = false
			err := writeKeyPolicy(ctx, w.deps, k)
			if !tc.accept {
				if err == nil {
					t.Fatal("the attach bypassed the reference guard")
				}
				return
			}
			if err != nil {
				t.Fatalf("writeKeyPolicy: %v", err)
			}
			got, err := w.stores.Key.Get(ctx, k.Meta.ID)
			if err != nil || got == nil {
				t.Fatalf("read back: %v", err)
			}
			if got.Spec.PolicyID != tc.policyID {
				t.Errorf("policyId = %q, want %q", got.Spec.PolicyID, tc.policyID)
			}
			if !got.Meta.Dirty {
				t.Error("the attach did not mark the row hand-edited; apply would overwrite it silently")
			}
		})
	}

	// Detaching clears the ref, and the cleared row still passes its own
	// validation — a policy-less key is a valid one.
	k.Spec.PolicyID = ""
	if err := writeKeyPolicy(ctx, w.deps, k); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got, err := w.stores.Key.Get(ctx, k.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("read back after detach: %v", err)
	}
	if got.Spec.PolicyID != "" {
		t.Errorf("policyId = %q, want it cleared", got.Spec.PolicyID)
	}

	// A row the guard lets through but that fails its own Validate is a 400,
	// not a store write.
	broken := *k
	broken.Spec.KeyHash = "too-short"
	if err := writeKeyPolicy(ctx, w.deps, &broken); statusOf(t, err) != 400 {
		t.Fatalf("err = %v, want a 400 from the row's own validation", err)
	}
}
