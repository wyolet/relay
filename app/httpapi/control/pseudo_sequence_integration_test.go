//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// The four kinds the way a team admin uses them, one after another: bring a provider key for the team, make an agent drawing on that account, narrow the agent's access to a provider's models, cap its spend. Each answer says what it did, and the plans end with no issue.
func TestPseudoKinds_InSequence(t *testing.T) {
	w := newHostKeyFormWorld(t, func(d *Deps) { d.PseudoKinds = []string{"all"} })
	send := func(path string, body any, out any) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(mustJSON(t, body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", "bob")
		rec := httptest.NewRecorder()
		w.r.ServeHTTP(rec, req)
		if rec.Code < 300 {
			must(t, "decode "+path, json.Unmarshal(rec.Body.Bytes(), out))
		}
		return rec
	}
	team := map[string]string{"kind": "team", "id": w.teamTwo.Meta.ID}
	priceAlphaModel(t, w)

	var key PseudoHostKeyCreated
	rec := send("/pseudo-host-keys", map[string]any{
		"metadata": map[string]any{"displayName": "Team key", "owner": team},
		"spec":     map[string]any{"hostId": w.alpha.Meta.ID, "value": keyOrgC, "account": map[string]any{"displayName": "Team C"}},
	}, &key)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff([]string{"account/team-c", "host-key/team-key"}, createdNames(key)); diff != "" || key.Detection != "detected" || key.Warnings == nil {
		t.Fatalf("host key: detection %q, warnings %v, created (-want +got):\n%s", key.Detection, key.Warnings, diff)
	}

	w.loadPlans(t)
	var agent PseudoAgentCreated
	rec = send("/pseudo-agents", map[string]any{
		"metadata": map[string]any{"displayName": "CI bot", "owner": map[string]string{"kind": "project", "id": w.proj.Meta.ID}},
		"spec":     map[string]any{"access": map[string]any{"accountIds": []string{key.Account.ID}}, "key": map[string]any{"displayName": "CI key"}},
	}, &agent)
	wantStatus(t, rec, http.StatusCreated)
	if len(agent.Pools) != 1 || len(agent.Shares) != 0 || agent.Policy == nil || agent.Key == nil || agent.Key.Plaintext == "" || agent.Warnings == nil {
		t.Fatalf("agent = %+v", agent)
	}
	sa := map[string]string{"kind": "service-account", "id": agent.ServiceAccount.ID}

	w.loadPlans(t)
	var access PseudoAccessAnswer
	rec = send("/pseudo-access", map[string]any{"spec": map[string]any{"target": sa, "access": map[string]any{"poolIds": []string{agent.Pools[0].ID}, "models": []string{"alpha-provider"}}}}, &access)
	wantStatus(t, rec, http.StatusCreated)
	wantAccess := PseudoAccessAnswer{Case: pseudo.CaseOwn, Policy: agent.Policy, Pools: agent.Pools,
		Updated: []CreatedRow{{Kind: "policy", ID: agent.Policy.ID, Name: agent.Policy.Name}}, Warnings: []PseudoIssue{}}
	if diff := cmp.Diff(wantAccess, access); diff != "" {
		t.Errorf("access (-want +got):\n%s", diff)
	}

	w.loadPlans(t)
	var limits PseudoLimitsAnswer
	rec = send("/pseudo-limits", map[string]any{"metadata": map[string]any{"displayName": "CI cap"}, "spec": map[string]any{"target": sa, "budget": map[string]any{"amountUsd": "50"}}}, &limits)
	wantStatus(t, rec, http.StatusCreated)
	wantLimits := PseudoLimitsAnswer{Case: pseudo.CaseOwn, Policy: agent.Policy, Budget: &RowRef{ID: limits.Budget.ID, Name: "ci-cap"},
		Created: []CreatedRow{{Kind: "budget", ID: limits.Budget.ID, Name: "ci-cap"}}, Updated: []CreatedRow{{Kind: "policy", ID: agent.Policy.ID, Name: agent.Policy.Name}}, Warnings: []PseudoIssue{}}
	if diff := cmp.Diff(wantLimits, limits); diff != "" {
		t.Errorf("limits (-want +got):\n%s", diff)
	}

	w.loadPlans(t)
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
}

// priceAlphaModel prices the host key world's one model at its host, so a budget refuses nothing for want of a price.
func priceAlphaModel(t *testing.T, w *hostKeyFormWorld) {
	t.Helper()
	ctx := t.Context()
	bindings, err := w.stores.Binding.List(ctx)
	must(t, "host bindings", err)
	for _, b := range bindings {
		if b.Spec.HostID != w.alpha.Meta.ID {
			continue
		}
		pr := &pricing.Pricing{
			Meta: meta.Metadata{ID: ids.New(), Name: "alpha-model-price", Owner: meta.Owner{Kind: meta.OwnerHost, ID: w.alpha.Meta.ID}},
			Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{b.Spec.ModelID}, Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1}}},
		}
		must(t, "pricing", w.stores.Pricing.Upsert(ctx, pr))
		b.Spec.PricingID = pr.Meta.ID
		must(t, "priced binding", w.stores.Binding.Upsert(ctx, b))
	}
	must(t, "reload", w.cat.Reload(ctx))
	w.loadPlans(t)
}
