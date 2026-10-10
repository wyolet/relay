//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/ids"
)

// seedHostTiers writes what a catalog seed leaves for one host: the host, a
// host-owned rate limit, and two host-owned tier policies naming it. Ids are
// minted per call, as each deployment's seed mints its own.
func seedHostTiers(t *testing.T, st *stack) (hostID, rateLimitID string, tiers map[string]string) {
	t.Helper()
	ctx := context.Background()
	h := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://acme.example.com"}}
	mustUpsert(t, st.stores.Host.Upsert(ctx, h), "host")
	owner := meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: ids.New(), Name: "acme-tier-rl", Owner: owner},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute), Strategy: "token-bucket"}}},
	}
	mustUpsert(t, st.stores.RateLimit.Upsert(ctx, rl), "rate limit")
	tiers = map[string]string{}
	for _, name := range []string{"acme-pristine", "acme-edited"} {
		p := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner}, Spec: policy.Spec{RateLimitID: rl.Meta.ID}}
		mustUpsert(t, st.stores.Policy.Upsert(ctx, p), "policy "+name)
		tiers[name] = p.Meta.ID
	}
	return h.Meta.ID, rl.Meta.ID, tiers
}

func exportedPolicies(t *testing.T, body []byte) map[string]*manifest.PolicyDTO {
	t.Helper()
	docs, err := manifest.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse export: %v\n%s", err, body)
	}
	out := map[string]*manifest.PolicyDTO{}
	for _, d := range docs {
		if d.Policy != nil {
			out[d.Policy.Metadata.Name] = d.Policy
		}
	}
	return out
}

// A backup has to carry what the operator made with the admin token and the
// catalog rows they edited, and nothing the catalog seed would recreate.
func TestIntegration_ExportIncludeSystem(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()

	_, rateLimitID, tiers := seedHostTiers(t, st)
	edit := `{"metadata":{"name":"acme-edited","displayName":"Edited by admin"},"spec":{"rateLimitId":"` + rateLimitID + `"}}`
	if code, raw := st.adminDo(http.MethodPut, "/api/policies/by-id/"+tiers["acme-edited"], edit); code != http.StatusOK {
		t.Fatalf("PUT catalog policy = %d: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"ci-crud"},"spec":{}}`); code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}

	code, body := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, body)
	}
	if got := exportedPolicies(t, body); len(got) != 0 {
		t.Fatalf("default export included non-tenant policies: %v", got)
	}

	code, body = st.adminDo(http.MethodGet, "/api/export?includeSystem=true", "")
	if code != http.StatusOK {
		t.Fatalf("export includeSystem = %d: %s", code, body)
	}
	got := exportedPolicies(t, body)
	if got["acme-pristine"] != nil {
		t.Fatalf("export included a pristine catalog row:\n%s", body)
	}
	if got["ci-crud"] == nil || got["acme-edited"] == nil {
		t.Fatalf("export missed an operator-made row:\n%s", body)
	}
	if o := got["acme-edited"].Metadata.Owner; o.Kind != meta.OwnerHost || o.Name != "acme-api" {
		t.Fatalf("host owner rendered as %+v, want the host's name", o)
	}

	// On the deployment it came from the bundle is a no-op once force lets
	// the dirty row through the diff.
	_, plan, raw := st.applyBundle(string(body), "dryRun=true&force=true")
	for _, e := range plan.Plan {
		if e.Action != "unchanged" {
			t.Fatalf("round trip reported %s on %s/%s (%v)\n%s", e.Action, e.Kind, e.Name, e.ChangedFields, raw)
		}
	}
	if plan.Counts.Unchanged != 2 {
		t.Fatalf("round trip counts = %+v", plan.Counts)
	}

	// Restored onto a fresh seed, whose ids differ, the edit lands on that
	// deployment's own catalog row and the admin-made row is recreated.
	fresh := newStack(t)
	freshHostID, freshRateLimitID, freshTiers := seedHostTiers(t, fresh)
	code, plan, raw = fresh.applyBundle(string(body), "")
	if code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, raw)
	}
	if plan.action("Policy", "acme-edited") != "update" || plan.action("Policy", "ci-crud") != "create" {
		t.Fatalf("restore plan = %+v", plan.Plan)
	}
	restored, err := fresh.stores.Policy.Get(ctx, freshTiers["acme-edited"])
	if err != nil || restored == nil {
		t.Fatalf("read restored policy: %v", err)
	}
	if restored.Meta.DisplayName != "Edited by admin" ||
		restored.Meta.Owner != (meta.Owner{Kind: meta.OwnerHost, ID: freshHostID}) ||
		restored.Spec.RateLimitID != freshRateLimitID {
		t.Fatalf("restored policy = %+v %+v", restored.Meta, restored.Spec)
	}
}
