//go:build integration

package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// bindAt binds model m at host id, so lanes reaching a key there are served it.
func (w *policyStepWorld) bindAt(t *testing.T, m *model.Model, hostID string) {
	t.Helper()
	must(t, "binding", w.stores.Binding.Upsert(t.Context(), &binding.Binding{Meta: meta.Metadata{ID: ids.New(), Name: m.Meta.Name + "-at-" + hostID[30:], Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: m.Meta.ID, HostID: hostID, Adapter: adapters.OpenAI}}))
	must(t, "reload", w.cat.Reload(t.Context()))
}

// Team two's lanes reach by ownership every key the team owns: m1 and m2 at the share host and m1 at the second host. Narrowing the team to its account at the share host loses m1 at the second host and gains nothing the lanes were not served at already, though a pool of the team's now carries them: narrowed only.
func TestPseudoAccess_UnderNarrowedOnly(t *testing.T) {
	w := newPolicyStepWorld(t)
	w.bindAt(t, w.m1, w.secondHostAcct.Spec.HostID)
	w.loadPlans(t)
	rec, out := w.access(t, "bob", target("team", w.teamTwo.Meta.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, true)
	wantStatus(t, rec, http.StatusOK)
	if out.Under == nil || out.Under.Lanes == 0 || out.Under.Narrowed != out.Under.Lanes || out.Under.Widened != 0 || out.Under.Changed != 0 {
		t.Errorf("under = %+v, want every lane narrowed and nothing else", out.Under)
	}
}

// Narrowing team two to a share of bob's account at a host the team did not reach loses the team's own hosts and gains that one: each lane both lost and gained, counted once as changed.
func TestPseudoAccess_UnderChanged(t *testing.T) {
	w := newPolicyStepWorld(t)
	ctx := t.Context()
	third := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "third-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://third.example.com"}}
	must(t, "third host", w.stores.Host.Upsert(ctx, third))
	bobs := &account.Account{Meta: meta.Metadata{ID: ids.New(), Name: "bob-third", Owner: meta.Owner{Kind: meta.OwnerUser, ID: w.users["bob"]}}, Spec: account.Spec{HostID: third.Meta.ID}}
	must(t, "bob's account", w.stores.Account.Upsert(ctx, bobs))
	must(t, "bob's key", w.stores.HostKey.Upsert(ctx, &hostkey.HostKey{Meta: meta.Metadata{ID: ids.New(), Name: "bob-third-key", Owner: bobs.Meta.Owner}, Spec: hostkey.Spec{HostID: third.Meta.ID, AccountID: bobs.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "PSEUDO_AGENT_TEST_KEY"}}}))
	w.bindAt(t, w.m1, third.Meta.ID)
	w.loadPlans(t)
	rec, out := w.access(t, "bob", target("team", w.teamTwo.Meta.ID), map[string]any{"accountIds": []string{bobs.Meta.ID}}, true)
	wantStatus(t, rec, http.StatusOK)
	if out.Under == nil || out.Under.Lanes == 0 || out.Under.Changed != out.Under.Lanes || out.Under.Narrowed != 0 || out.Under.Widened != 0 {
		t.Errorf("under = %+v, want every lane changed and nothing else", out.Under)
	}
}

// A lane under the target the caller may not see that becomes unreachable is counted in unreachableHidden, neither its id nor its name answered.
func TestPseudoAccess_UnderHidesLanes(t *testing.T) {
	w := newPolicyStepWorld(t)
	must(t, "m1 at the second host", w.stores.Binding.Upsert(t.Context(), &binding.Binding{Meta: meta.Metadata{ID: ids.New(), Name: "m1-at-second-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: w.m1.Meta.ID, HostID: w.secondHostAcct.Spec.HostID, Adapter: adapters.OpenAI}}))
	must(t, "reload", w.cat.Reload(t.Context()))
	w.loadPlans(t)
	var agent PseudoAgentCreated
	wantStatus(t, w.postPseudo(t, "bob", "/pseudo-agents", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}}), false, &agent), http.StatusCreated)
	w.loadPlans(t)
	w.hidden[agent.ServiceAccount.ID] = true
	rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, true)
	wantStatus(t, rec, http.StatusOK)
	if diff := cmp.Diff(&PseudoUnder{Lanes: 1, Narrowed: 1, Unreachable: []UnderLane{}, UnreachableHidden: 1}, out.Under); diff != "" {
		t.Errorf("under (-want +got):\n%s", diff)
	}
	for _, s := range []string{agent.Account.ID, agent.Account.Name, agent.ServiceAccount.ID} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("the answer names %q of a lane the caller may not see", s)
		}
	}
}

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
	sibLane, err := w.stores.Account.EnsureManaged(ctx, account.ForServiceAccount(sibSA.Meta.ID, sibSA.Meta.Name, "", sibSA.Meta.Owner))
	must(t, "sibling lane", err)
	must(t, "reload", w.cat.Reload(ctx))
	w.loadPlans(t)

	var agent PseudoAgentCreated
	rec := w.postPseudo(t, "bob", "/pseudo-agents", w.agentBody(map[string]any{"access": map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}}), false, &agent)
	wantStatus(t, rec, http.StatusCreated)
	w.loadPlans(t)
	sibReach := w.reach(t, sibLane)

	want := &PseudoUnder{Lanes: 1, Narrowed: 1, Unreachable: []UnderLane{{Kind: "service-account", ID: agent.ServiceAccount.ID, Name: "CI bot"}}}
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
