//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// everyCredential on a policy that links a pool the caller may not see would leave the account reaching only that pool: refused with 409 hidden_pools_kept, nothing written.
func TestPseudoAccess_EveryCredentialWithHiddenPools(t *testing.T) {
	w := newPolicyStepWorld(t)
	hidden := w.hiddenPool(t)
	w.bind(t, w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID, hidden.Meta.ID}}), w.owner, w.projTwoOwn)
	w.loadPlans(t)
	before := w.rowCounts(t)
	rec, _ := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"everyCredential": true}, false)
	if r := refusalOf(t, rec); rec.Code != http.StatusConflict || r.Err.Code != codeHiddenPoolsKept || r.Pointer != "/spec/access/everyCredential" {
		t.Errorf("%d %s, want 409 %s", rec.Code, rec.Body, codeHiddenPoolsKept)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote (-before +after):\n%s", diff)
	}
}

// The prefill says whether the binding holding the account is enabled: a disabled one binds nothing, so its policy is not the one governing the account.
func TestPseudoAccess_PrefillBindingEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		w := newPolicyStepWorld(t)
		b := w.bind(t, w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}}), w.owner, w.projTwoOwn)
		b.Spec.Enabled = &enabled
		must(t, "binding", w.stores.PolicyBinding.Upsert(t.Context(), b))
		w.loadPlans(t)
		rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), nil, true)
		wantStatus(t, rec, http.StatusOK)
		if out.Current == nil || out.Current.BindingEnabled != enabled {
			t.Errorf("enabled %v: current = %+v", enabled, out.Current)
		}
	}
}
