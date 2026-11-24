//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// ownersWorld is the host key world with every pseudo kind on and a personal account for each user, as relay's account writer keeps them.
func ownersWorld(t *testing.T) *hostKeyFormWorld {
	t.Helper()
	w := newHostKeyFormWorld(t, func(d *Deps) { d.PseudoKinds = []string{"all"} })
	for name, id := range w.users {
		_, err := w.stores.Account.EnsureManaged(t.Context(), account.Personal(id, name))
		must(t, "personal account", err)
	}
	priceAlphaModel(t, w)
	return w
}

// sendAs posts body to path as who and decodes a success into out.
func (w *hostKeyFormWorld) sendAs(t *testing.T, who, path string, body, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(mustJSON(t, body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", who)
	rec := httptest.NewRecorder()
	w.r.ServeHTTP(rec, req)
	if rec.Code < 300 && out != nil {
		must(t, "decode "+path, json.Unmarshal(rec.Body.Bytes(), out))
	}
	return rec
}

// An agent owned by a team, and one owned by a user, go through the four service-account kinds as a project's does: made with its pools and policy in the owner's scope and no share of the owner's own account, its access and limits changed in place, then removed with them. The user's agent climbs the user's personal account; neither leaves a plan issue.
func TestPseudoKinds_TeamAndUserOwnedAgents(t *testing.T) {
	for _, tc := range []struct {
		name, who string
		owner     func(w *hostKeyFormWorld) meta.Owner
	}{
		{"team", "bob", func(w *hostKeyFormWorld) meta.Owner { return meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID} }},
		{"user", "carol", func(w *hostKeyFormWorld) meta.Owner { return meta.Owner{Kind: meta.OwnerUser, ID: w.users["carol"]} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ownersWorld(t)
			ctx := t.Context()
			owner := tc.owner(w)
			ownerBody := map[string]string{"kind": string(owner.Kind), "id": owner.ID}

			var key PseudoHostKeyCreated
			rec := w.sendAs(t, tc.who, "/pseudo-host-keys", map[string]any{
				"metadata": map[string]any{"displayName": "Own key", "owner": ownerBody},
				"spec":     map[string]any{"hostId": w.alpha.Meta.ID, "value": keyOrgC, "account": map[string]any{"displayName": "Own C"}},
			}, &key)
			wantStatus(t, rec, http.StatusCreated)

			w.loadPlans(t)
			var agent PseudoAgentCreated
			rec = w.sendAs(t, tc.who, "/pseudo-agents", map[string]any{
				"metadata": map[string]any{"displayName": "Own bot", "owner": ownerBody},
				"spec":     map[string]any{"access": map[string]any{"accountIds": []string{key.Account.ID}}, "key": map[string]any{"displayName": "Own bot key"}},
			}, &agent)
			wantStatus(t, rec, http.StatusCreated)
			if len(agent.Pools) != 1 || len(agent.Shares) != 0 || agent.Policy == nil || agent.Key == nil || agent.Key.Plaintext == "" {
				t.Fatalf("agent = %+v", agent)
			}
			sa, err := w.stores.ServiceAccount.Get(ctx, agent.ServiceAccount.ID)
			must(t, "service account", err)
			lane, err := w.stores.Account.Get(ctx, agent.Account.ID)
			must(t, "lane", err)
			pl, err := w.stores.Pool.Get(ctx, agent.Pools[0].ID)
			must(t, "pool", err)
			pol, err := w.stores.Policy.Get(ctx, agent.Policy.ID)
			must(t, "policy", err)
			if got := []meta.Owner{sa.Meta.Owner, lane.Meta.Owner, pl.Meta.Owner, pol.Meta.Owner}; !cmp.Equal(got, []meta.Owner{owner, owner, owner, owner}) {
				t.Errorf("owners of the service account, lane, pool and policy = %+v, want %+v", got, owner)
			}

			w.loadPlans(t)
			cp := plan.LanePlan(w.plans.View(), agent.Account.ID)
			wantTop := limits.RungTeam
			if owner.Kind == meta.OwnerUser {
				wantTop = limits.RungPersonal
			}
			if cp == nil || cp.Refused != nil || cp.Team().Kind != wantTop || cp.Team().SubjectID == "" || cp.Project().SubjectID != "" {
				t.Fatalf("lane plan = %+v, want %s above the lane and no project", cp, wantTop)
			}
			sat := map[string]string{"kind": "service-account", "id": agent.ServiceAccount.ID}
			var access PseudoAccessAnswer
			rec = w.sendAs(t, tc.who, "/pseudo-access", map[string]any{"spec": map[string]any{"target": sat, "access": map[string]any{"poolIds": []string{agent.Pools[0].ID}, "models": []string{"alpha-provider"}}}}, &access)
			wantStatus(t, rec, http.StatusCreated)
			wantAccess := PseudoAccessAnswer{Case: pseudo.CaseOwn, Policy: agent.Policy, Pools: agent.Pools,
				Updated: []CreatedRow{{Kind: "policy", ID: agent.Policy.ID, Name: agent.Policy.Name}}, Warnings: []PseudoIssue{}}
			if diff := cmp.Diff(wantAccess, access); diff != "" {
				t.Errorf("access (-want +got):\n%s", diff)
			}

			w.loadPlans(t)
			var lim PseudoLimitsAnswer
			rec = w.sendAs(t, tc.who, "/pseudo-limits", map[string]any{"metadata": map[string]any{"displayName": "Own cap"}, "spec": map[string]any{"target": sat, "budget": map[string]any{"amountUsd": "50"}}}, &lim)
			wantStatus(t, rec, http.StatusCreated)
			if lim.Case != pseudo.CaseOwn || lim.Budget == nil || len(lim.Created) != 1 {
				t.Fatalf("limits = %+v", lim)
			}
			b, err := w.stores.Budget.Get(ctx, lim.Budget.ID)
			if err != nil || b == nil || b.Meta.Owner != owner {
				t.Errorf("budget = %+v (%v), want one owned by %+v", b, err, owner)
			}

			// A limit counted per lane changes no other account's reach: for a user's agent it counts in the user's personal lane, which the guard does not read as a change of that lane.
			w.loadPlans(t)
			each := false
			var perLane PseudoLimitsAnswer
			rec = w.sendAs(t, tc.who, "/pseudo-limits", map[string]any{"metadata": map[string]any{"displayName": "Own lane cap"}, "spec": map[string]any{"target": sat, "budget": map[string]any{"amountUsd": "5"}, "link": map[string]any{"shared": each}}}, &perLane)
			wantStatus(t, rec, http.StatusCreated)
			w.loadPlans(t)
			wantLane := "account:" + agent.Account.ID
			if owner.Kind == meta.OwnerUser {
				wantLane = plan.LanePlan(w.plans.View(), agent.Account.ID).Team().SubjectID
			}
			var lanes []string
			for _, c := range plan.LanePlan(w.plans.View(), agent.Account.ID).Counters {
				if c.Budget != nil && c.Budget.Meta.ID == perLane.Budget.ID {
					lanes = append(lanes, c.Lane)
				}
			}
			if !cmp.Equal(lanes, []string{wantLane}) {
				t.Errorf("per-lane budget counts in %v, want %s", lanes, wantLane)
			}

			w.loadPlans(t)
			hash := apikey.Hash(agent.Key.Plaintext)
			if engine.Get[plan.KeyPlanIndex](w.plans.View(), "").Lookup(hash, time.Now()) == nil {
				t.Fatal("the agent's key does not authenticate before the removal")
			}
			var dry, removal PseudoAgentRemovalAnswer
			wantStatus(t, w.sendAs(t, tc.who, "/pseudo-agent-removals?dryRun=true", map[string]any{"spec": map[string]any{"serviceAccountId": agent.ServiceAccount.ID}}, &dry), http.StatusOK)
			rec = w.sendAs(t, tc.who, "/pseudo-agent-removals", map[string]any{"spec": map[string]any{"serviceAccountId": agent.ServiceAccount.ID, "expect": idsOf(dry.Deleted)}}, &removal)
			wantStatus(t, rec, http.StatusOK)
			var removed []string
			for _, r := range removal.Deleted {
				removed = append(removed, r.Kind+"/"+r.ID)
			}
			wantRemoved := []string{"key/" + agent.Key.ID, "policy-binding/" + removal.Deleted[1].ID, "service-account/" + agent.ServiceAccount.ID, "account/" + agent.Account.ID, "policy/" + agent.Policy.ID, "pool/" + agent.Pools[0].ID}
			if diff := cmp.Diff(wantRemoved, removed); diff != "" || len(removal.Kept) != 0 {
				t.Errorf("removal kept %v, deleted (-want +got):\n%s", removal.Kept, diff)
			}
			if a, err := w.stores.Account.Get(ctx, key.Account.ID); err != nil || a == nil {
				t.Errorf("the owner's provider account is gone: %v", err)
			}

			w.loadPlans(t)
			if kp := engine.Get[plan.KeyPlanIndex](w.plans.View(), "").Lookup(hash, time.Now()); kp != nil {
				t.Errorf("the deleted key still authenticates as %+v", kp)
			}
			req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
			req.Header.Set("X-Test-User", "root")
			got := httptest.NewRecorder()
			w.r.ServeHTTP(got, req)
			wantStatus(t, got, http.StatusOK)
			var issues []PlanIssue
			must(t, "decode plan issues", json.Unmarshal(got.Body.Bytes(), &issues))
			if len(issues) != 0 {
				t.Errorf("plan issues at the end: %s", got.Body)
			}
		})
	}
}

// A user's agent sits under the user's personal account: the plan check warns of a model the personal policy above it leaves out, and the user still changes the agent's own policy in place under that policy. A binding of the agent's account an admin made in the organization is above the user, whose change is refused naming the organization.
func TestPseudoAgents_UnderThePersonalAccount(t *testing.T) {
	w := ownersWorld(t)
	ctx := t.Context()
	carolID := w.users["carol"]
	carol := map[string]string{"kind": "user", "id": carolID}
	provs, err := w.stores.Provider.List(ctx)
	must(t, "providers", err)
	other := &model.Model{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-other", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provs[0].Meta.ID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "alpha-other"}}, Pointer: "alpha-other"}}
	must(t, "model", w.stores.Model.Upsert(ctx, other))
	must(t, "binding", w.stores.Binding.Upsert(ctx, &binding.Binding{Meta: meta.Metadata{ID: ids.New(), Name: "alpha-other-at-alpha", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: other.Meta.ID, HostID: w.alpha.Meta.ID, Adapter: adapters.OpenAI}}))
	personalID, err := w.stores.Account.EnsureManaged(ctx, account.Personal(carolID, "carol"))
	must(t, "personal account", err)
	onlyOther := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "carol-only-other", Owner: meta.Global}, Spec: policy.Spec{Models: []string{"alpha-provider/alpha-other"}}}
	must(t, "personal policy", w.stores.Policy.Upsert(ctx, onlyOther))
	must(t, "personal binding", w.stores.PolicyBinding.Upsert(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: "carol-personal", Owner: meta.Global}, Spec: policybinding.Spec{PolicyID: onlyOther.Meta.ID, AccountIDs: []string{personalID}}}))
	must(t, "reload", w.cat.Reload(ctx))

	// A user's agent climbs no team, so a change on a team counts the same lanes under it before and after one exists.
	underTeam := func() int {
		t.Helper()
		w.loadPlans(t)
		var a PseudoAccessAnswer
		wantStatus(t, w.sendAs(t, "bob", "/pseudo-access?dryRun=true", map[string]any{"spec": map[string]any{"target": map[string]string{"kind": "team", "id": w.teamTwo.Meta.ID}, "access": map[string]any{"everyCredential": true, "models": []string{"alpha-provider"}}}}, &a), http.StatusOK)
		if a.Under == nil {
			t.Fatalf("team access dry run answers no lanes under it: %+v", a)
		}
		return a.Under.Lanes
	}
	before := underTeam()

	var key PseudoHostKeyCreated
	wantStatus(t, w.sendAs(t, "carol", "/pseudo-host-keys", map[string]any{
		"metadata": map[string]any{"displayName": "Carol key", "owner": carol},
		"spec":     map[string]any{"hostId": w.alpha.Meta.ID, "value": keyOrgC, "account": map[string]any{"displayName": "Carol C"}},
	}, &key), http.StatusCreated)
	w.loadPlans(t)
	var agent PseudoAgentCreated
	wantStatus(t, w.sendAs(t, "carol", "/pseudo-agents", map[string]any{
		"metadata": map[string]any{"displayName": "Carol bot", "owner": carol},
		"spec":     map[string]any{"access": map[string]any{"accountIds": []string{key.Account.ID}, "models": []string{"alpha-provider/alpha-model"}}},
	}, &agent), http.StatusCreated)
	if !slices.ContainsFunc(agent.Warnings, func(i PseudoIssue) bool { return i.Code == plan.IssueModelRefBlocked }) {
		t.Errorf("agent warnings = %+v, want %s for the model the personal policy leaves out", agent.Warnings, plan.IssueModelRefBlocked)
	}

	w.loadPlans(t)
	sat := map[string]string{"kind": "service-account", "id": agent.ServiceAccount.ID}
	var access PseudoAccessAnswer
	wantStatus(t, w.sendAs(t, "carol", "/pseudo-access", map[string]any{"spec": map[string]any{"target": sat, "access": map[string]any{"poolIds": []string{agent.Pools[0].ID}, "models": []string{"alpha-provider"}}}}, &access), http.StatusCreated)
	if access.Case != pseudo.CaseOwn {
		t.Errorf("access case = %s, want %s: the personal policy above is not the agent's own", access.Case, pseudo.CaseOwn)
	}
	if after := underTeam(); after != before {
		t.Errorf("lanes under team two = %d with carol's agent, %d without; want the same", after, before)
	}

	var bare PseudoAgentCreated
	wantStatus(t, w.sendAs(t, "carol", "/pseudo-agents", map[string]any{"metadata": map[string]any{"displayName": "Carol bare", "owner": carol}, "spec": map[string]any{"access": map[string]any{"everyCredential": true}}}, &bare), http.StatusCreated)
	orgPolicy := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "org-bots", Owner: meta.Global}}
	must(t, "org policy", w.stores.Policy.Upsert(ctx, orgPolicy))
	must(t, "org binding", w.stores.PolicyBinding.Upsert(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: "org-bots-carol-bare", Owner: meta.Global}, Spec: policybinding.Spec{PolicyID: orgPolicy.Meta.ID, AccountIDs: []string{bare.Account.ID}}}))
	must(t, "reload", w.cat.Reload(ctx))
	w.loadPlans(t)
	rec := w.sendAs(t, "carol", "/pseudo-access", map[string]any{"spec": map[string]any{"target": map[string]string{"kind": "service-account", "id": bare.ServiceAccount.ID}, "access": map[string]any{"everyCredential": true, "models": []string{"alpha-provider"}}}}, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), codePolicySetAbove) || !strings.Contains(rec.Body.String(), "the organization") {
		t.Errorf("carol changes the access an admin bound = %d %s, want 403 %s naming the organization", rec.Code, rec.Body, codePolicySetAbove)
	}
}

// A user's agent is the user's to make: another user is refused as POST /service-accounts refuses them, the admin token may make one for any user, and while the user's personal accounts are off its key is refused. Another user, a team admin, can neither change nor remove it.
func TestPseudoAgents_UserOwnerRefusals(t *testing.T) {
	w := ownersWorld(t)
	ctx := t.Context()
	carol := map[string]string{"kind": "user", "id": w.users["carol"]}
	body := func(name string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"displayName": name, "owner": carol},
			"spec":     map[string]any{"access": map[string]any{"everyCredential": true, "models": []string{"alpha-provider"}}, "key": map[string]any{"displayName": name + " key"}},
		}
	}

	rec := w.sendAs(t, "bob", "/pseudo-agents", body("Bob for carol"), nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "owner.id must be empty or match the calling user") {
		t.Errorf("bob makes carol's agent = %d %s, want 400", rec.Code, rec.Body)
	}

	var made PseudoAgentCreated
	wantStatus(t, w.sendAs(t, "root", "/pseudo-agents", body("Made for carol"), &made), http.StatusCreated)
	sat := map[string]string{"kind": "service-account", "id": made.ServiceAccount.ID}
	w.loadPlans(t)
	for path, b := range map[string]map[string]any{
		"/pseudo-access":         {"spec": map[string]any{"target": sat, "access": map[string]any{"everyCredential": true}}},
		"/pseudo-limits":         {"metadata": map[string]any{"displayName": "cap"}, "spec": map[string]any{"target": sat, "budget": map[string]any{"amountUsd": "5"}}},
		"/pseudo-agent-removals": {"spec": map[string]any{"serviceAccountId": made.ServiceAccount.ID}},
	} {
		if rec := w.sendAs(t, "bob", path, b, nil); rec.Code != http.StatusNotFound {
			t.Errorf("bob on carol's agent at %s = %d %s, want 404", path, rec.Code, rec.Body)
		}
	}

	if ok, err := user.NewStore(w.pool).SetPersonalAccount(ctx, w.users["carol"], settings.PersonalAccountsOff); err != nil || !ok {
		t.Fatalf("turn carol's personal accounts off: %v %v", ok, err)
	}
	w.loadPlans(t)
	before, err := w.stores.ServiceAccount.List(ctx)
	must(t, "service accounts", err)
	keyless := body("Carol off")
	delete(keyless["spec"].(map[string]any), "key")
	for name, b := range map[string]map[string]any{"with a key": body("Carol off"), "without a key": keyless} {
		for _, path := range []string{"/pseudo-agents?dryRun=true", "/pseudo-agents"} {
			rec := w.sendAs(t, "carol", path, b, nil)
			if r := refusalOf(t, rec); rec.Code != http.StatusForbidden || r.Err.Code != "personal_account_off" || r.Pointer != "/metadata/owner" {
				t.Errorf("%s at %s while carol's personal accounts are off = %d %s at %q, want 403 personal_account_off at /metadata/owner", name, path, rec.Code, r.Err.Code, r.Pointer)
			}
		}
	}
	if after, err := w.stores.ServiceAccount.List(ctx); err != nil || len(after) != len(before) {
		t.Errorf("service accounts %d after the refusals, %d before (%v)", len(after), len(before), err)
	}
}
