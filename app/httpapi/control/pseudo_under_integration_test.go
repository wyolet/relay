//go:build integration

package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Setting a project's access to one account at the share host leaves an agent under it, whose own policy links only a pool at the second host, serving nothing. That is what the change means, so it is not refused: the dry run and the save answer the agent in under.unreachable. A project beside it in the same team, outside the target, reaches what it did.
func TestPseudoAccess_UnderTheTarget(t *testing.T) {
	w := newPolicyStepWorld(t)
	ctx := t.Context()
	must(t, "m1 at the second host", w.stores.Binding.Upsert(ctx, &binding.Binding{Meta: meta.Metadata{ID: ids.New(), Name: "m1-at-second-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: w.m1.Meta.ID, HostID: w.secondHostAcct.Spec.HostID, Adapter: adapters.OpenAI}}))
	sib := &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: "two-other"}, Spec: project.Spec{TeamID: w.teamTwo.Meta.ID}}
	sib.StampOwner()
	must(t, "sibling project", w.stores.Project.Upsert(ctx, sib))
	sibSA := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "other-bot"}, Spec: serviceaccount.Spec{ProjectID: sib.Meta.ID}}
	sibSA.StampOwner()
	must(t, "sibling service account", w.stores.ServiceAccount.Upsert(ctx, sibSA))
	sibLane, err := w.stores.Account.EnsureManaged(ctx, account.ForServiceAccount(sibSA.Meta.ID, sibSA.Meta.Name, "", sib.Meta.ID))
	must(t, "sibling lane", err)
	must(t, "reload", w.cat.Reload(ctx))
	w.loadPlans(t)

	var agent PseudoAgentCreated
	rec := w.postPseudo(t, "bob", "/pseudo-agents", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}}), false, &agent)
	wantStatus(t, rec, http.StatusCreated)
	w.loadPlans(t)
	sibReach := w.reach(t, sibLane)

	want := &PseudoUnder{Lanes: 1, Narrowed: 1, Unreachable: []UnderLane{{Kind: "account", ID: agent.Account.ID, Name: agent.Account.Name}}}
	tgt := target("project", w.projTwo.Meta.ID)
	access := map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}
	rec, dry := w.access(t, "bob", tgt, access, true)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(want, dry.Under); diff != "" {
		t.Errorf("dry run under (-want +got):\n%s", diff)
	}
	rec, saved := w.access(t, "bob", tgt, access, false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff(want, saved.Under); diff != "" {
		t.Errorf("save under (-want +got):\n%s", diff)
	}
	w.loadPlans(t)
	if got := w.reach(t, sibLane); got != sibReach {
		t.Errorf("the sibling project's lane reaches other rows:\nbefore %s\nafter  %s", sibReach, got)
	}

	rec, out := w.access(t, "bob", target("service-account", agent.ServiceAccount.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, true)
	wantStatus(t, rec, http.StatusOK)
	if out.Under != nil {
		t.Errorf("a service account target answered under %+v", out.Under)
	}
}
