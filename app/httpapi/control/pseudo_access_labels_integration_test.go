//go:build integration

package control

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// labelledFor is a pool of owner at the share host drawing on team two's account, labelled as the access kind labels the pools it makes for the account.
func (w *policyStepWorld) labelledFor(t *testing.T, accountID string, owner meta.Owner) *pool.Pool {
	t.Helper()
	p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "labelled-" + ids.New()[30:], Owner: owner, Labels: map[string]string{pseudo.AccessLabel: accountID}}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamTwoAcct.Meta.ID}}}
	p.Default()
	must(t, "labelled pool", w.stores.Pool.Upsert(t.Context(), p))
	return w.storedPool(t, p.Meta.ID)
}

// A label is a claim anyone who makes a pool can write. A labelled pool of another scope is not the kind's: it is neither reused nor rewritten, and a new pool is made in the target's scope.
func TestPseudoAccess_LabelledPoolOfAnotherScope(t *testing.T) {
	w := newPolicyStepWorld(t)
	spoof := w.labelledFor(t, w.projTwoOwn.Meta.ID, w.two)
	w.loadPlans(t)
	rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Pools) != 1 || out.Pools[0].ID == spoof.Meta.ID {
		t.Errorf("pools = %v, want one new pool, not %s", out.Pools, spoof.Meta.Name)
	}
	if diff := cmp.Diff(spoof, w.storedPool(t, spoof.Meta.ID)); diff != "" {
		t.Errorf("the other scope's pool changed (-before +after):\n%s", diff)
	}
}

// A labelled pool of the target's scope the caller may update but not delete: when the policy stops linking it, it is unlinked and kept, and the request goes through.
func TestPseudoAccess_LabelledPoolNotDeletable(t *testing.T) {
	w := newPolicyStepWorld(t)
	own := w.labelledFor(t, w.projTwoOwn.Meta.ID, w.owner)
	p := w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{own.Meta.ID}})
	w.bind(t, p, w.owner, w.projTwoOwn)
	w.loadPlans(t)
	*w.denied = "pools.delete"
	defer func() { *w.denied = "" }()
	rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Deleted) != 0 || w.storedPool(t, own.Meta.ID) == nil {
		t.Errorf("deleted %v; the pool the caller may not delete must stay", out.Deleted)
	}
	if got := w.heldPolicy(t, w.projTwoOwn.Meta.ID); got == nil || slices.Contains(got.Spec.PoolIDs, own.Meta.ID) {
		t.Errorf("policy = %+v, want the pool unlinked", got)
	}
}

// A labelled pool of the target's scope that a second policy links is not the kind's: it is neither reused nor deleted, and the prefill does not flag it as the kind's.
func TestPseudoAccess_LabelledPoolLinkedElsewhere(t *testing.T) {
	w := newPolicyStepWorld(t)
	shared := w.labelledFor(t, w.projTwoOwn.Meta.ID, w.owner)
	w.bind(t, w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{shared.Meta.ID}}), w.owner, w.projTwoOwn)
	w.policy(t, "two-other", w.two, policy.Spec{PoolIDs: []string{shared.Meta.ID}})
	w.loadPlans(t)
	tgt := target("project", w.projTwo.Meta.ID)

	rec, pre := w.access(t, "bob", tgt, nil, true)
	wantStatus(t, rec, http.StatusOK)
	if pre.Current == nil || len(pre.Current.Pools) != 1 || pre.Current.Pools[0].Labelled {
		t.Errorf("prefill = %+v, want the pool not flagged as the kind's", pre.Current)
	}
	rec, out := w.access(t, "bob", tgt, map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Pools) != 1 || out.Pools[0].ID == shared.Meta.ID || len(out.Deleted) != 0 {
		t.Errorf("pools %v, deleted %v: the pool another policy links was reused or deleted", out.Pools, out.Deleted)
	}
	if diff := cmp.Diff(shared, w.storedPool(t, shared.Meta.ID)); diff != "" {
		t.Errorf("the shared pool changed (-before +after):\n%s", diff)
	}
}
