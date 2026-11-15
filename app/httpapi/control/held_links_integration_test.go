//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// A team admin saves and copies a team policy an admin linked to an organization rate limit, and saves a binding an admin pointed at an organization policy, though they see neither; linking one anew stays refused.
func TestSavesKeepLinksToUnseenOrganizationRows(t *testing.T) {
	w := newPolicyWorld(t)
	ctx := t.Context()
	org := meta.Owner{Kind: meta.OwnerOrganization}
	teamOne := meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}
	orgRL := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: ids.New(), Name: "org-rpm", Owner: org},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow}}},
	}
	must(t, "org rate limit", w.stores.RateLimit.Upsert(ctx, orgRL))
	orgPol := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "org-default", Owner: org}}
	must(t, "org policy", w.stores.Policy.Upsert(ctx, orgPol))
	w.polA.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: orgRL.Meta.ID}}
	must(t, "link pol-a", w.stores.Policy.Upsert(ctx, w.polA))
	w.shared.Spec.PolicyID = orgPol.Meta.ID
	must(t, "bind the org policy", w.stores.PolicyBinding.Upsert(ctx, w.shared))
	must(t, "reload", w.cat.Reload(ctx))

	links := `"rateLimits":[{"rateLimitId":"` + orgRL.Meta.ID + `"}]`
	wantStatus(t, w.send("dave", http.MethodPut, "/policies/by-id/"+w.polA.Meta.ID, `{"metadata":{"name":"pol-a","displayName":"Team A"},"spec":{`+links+`}}`), http.StatusOK)
	wantStatus(t, w.send("dave", http.MethodPut, "/policies/by-id/"+w.polB.Meta.ID, `{"metadata":{"name":"pol-b"},"spec":{`+links+`}}`), http.StatusNotFound)
	wantStatus(t, w.send("dave", http.MethodPost, "/policies/pol-a/duplicate", `{"displayName":"Team A copy","owner":{"kind":"team","id":"`+w.teamOne.Meta.ID+`"}}`), http.StatusCreated)

	raw, err := json.Marshal(w.shared.Spec.AccountIDs)
	must(t, "account ids", err)
	accounts := string(raw)
	binding := func(policyID string) string {
		return `{"metadata":{"name":"team-one-shared","displayName":"Shared","owner":{"kind":"team","id":"` + teamOne.ID + `"}},"spec":{"policyId":"` + policyID + `","accountIds":` + accounts + `}}`
	}
	wantStatus(t, w.send("dave", http.MethodPut, "/policy-bindings/by-id/"+w.shared.Meta.ID, binding(orgPol.Meta.ID)), http.StatusOK)
	other := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "org-other", Owner: org}}
	must(t, "another org policy", w.stores.Policy.Upsert(ctx, other))
	must(t, "reload", w.cat.Reload(ctx))
	wantStatus(t, w.send("dave", http.MethodPut, "/policy-bindings/by-id/"+w.shared.Meta.ID, binding(other.Meta.ID)), http.StatusNotFound)
}
