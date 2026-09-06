package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// audit 2026-07-04 P1 #7: disable→enable round-trips permanently lose
// dependents — re-enabling a model must restore the grant its policy still
// carries in PG.
func TestApply_ModelDisableEnableRestoresPolicyGrant(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m := models[0]
	pol := pols[0]

	inPolicy := func(s *Snapshot) bool {
		for _, mm := range s.ModelsInPolicy(pol.Meta.ID) {
			if mm.Meta.ID == m.Meta.ID {
				return true
			}
		}
		return false
	}
	if !inPolicy(c.Current()) {
		t.Fatal("precondition: model not granted by policy before the round-trip")
	}

	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatalf("ApplyModelDelete: %v", err)
	}
	if inPolicy(c.Current()) {
		t.Fatal("precondition: model still granted while deleted")
	}

	restored := &model.Model{Meta: m.Meta, Spec: m.Spec}
	if err := c.ApplyModelUpsert(restored); err != nil {
		t.Fatalf("ApplyModelUpsert: %v", err)
	}

	s := c.Current()
	if _, ok := s.Model(m.Meta.ID); !ok {
		t.Fatal("model missing after re-enable")
	}
	if !inPolicy(s) {
		t.Errorf("re-enabled model %q not restored to policy %q grants (ModelsInPolicy)", m.Meta.ID, pol.Meta.Name)
	}
	got, ok := s.Policy(pol.Meta.ID)
	if !ok {
		t.Fatal("policy missing")
	}
	found := false
	for _, id := range got.Spec.ModelIDs {
		if id == m.Meta.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("re-enabled model %q not restored to snapshot policy Spec.ModelIDs %v", m.Meta.ID, got.Spec.ModelIDs)
	}
}

// audit 2026-07-04 P1 #7: disable→enable round-trips permanently lose
// dependents — re-enabling a policy must restore the relay keys that still
// point at it in PG.
func TestApply_PolicyDisableEnableRestoresKeys(t *testing.T) {
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	pol := pols[0]
	hash := strings.Repeat("a", 64)
	if k, _ := c.Current().KeyByHash(hash); k == nil {
		t.Fatal("precondition: relay key not resolvable before the round-trip")
	}

	if err := c.ApplyPolicyDelete(pol.Meta.ID); err != nil {
		t.Fatalf("ApplyPolicyDelete: %v", err)
	}
	if k, _ := c.Current().KeyByHash(hash); k != nil {
		t.Fatal("precondition: relay key survived its policy's delete")
	}

	restored := &policy.Policy{Meta: pol.Meta, Spec: pol.Spec}
	if err := c.ApplyPolicyUpsert(restored); err != nil {
		t.Fatalf("ApplyPolicyUpsert: %v", err)
	}

	s := c.Current()
	if _, ok := s.Policy(pol.Meta.ID); !ok {
		t.Fatal("policy missing after re-enable")
	}
	if k, _ := s.KeyByHash(hash); k == nil {
		t.Errorf("relay key on policy %q not restored after policy re-enable — KeyByHash misses", pol.Meta.Name)
	}
}
