//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Giving one service account of project two bob's account shares it with the project, but a share reaches no lane by ownership: a lane reaches the keys its scopes own, and the share's keys stay bob's. A sibling service account's lane and the project's own account reach what they reached before, so the after-check lets the request through.
func TestPseudoAccess_ShareLeavesSiblingsAlone(t *testing.T) {
	w := newPolicyStepWorld(t)
	ctx := t.Context()
	sib := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "two-sibling"}, Spec: serviceaccount.Spec{ProjectID: w.projTwo.Meta.ID}}
	sib.StampOwner()
	must(t, "sibling", w.stores.ServiceAccount.Upsert(ctx, sib))
	lane, err := w.stores.Account.EnsureManaged(ctx, account.ForServiceAccount(sib.Meta.ID, sib.Meta.Name, "", sib.Meta.Owner))
	must(t, "sibling lane", err)
	w.loadPlans(t)
	before := map[string]string{lane: w.reach(t, lane), w.projTwoOwn.Meta.ID: w.reach(t, w.projTwoOwn.Meta.ID)}

	rec, out := w.access(t, "bob", target("service-account", w.sa.Meta.ID), map[string]any{"accountIds": []string{w.bobAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(out.Shares) != 1 {
		t.Fatalf("shares %v, want one", out.Shares)
	}
	w.loadPlans(t)
	for id, was := range before {
		if is := w.reach(t, id); is != was {
			t.Errorf("account %s reaches other rows after the share:\nbefore %s\nafter  %s", id, was, is)
		}
	}
}
