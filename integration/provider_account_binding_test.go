//go:build integration

// provider_account_binding_test.go binds a policy to a provider account and checks it counts across every key of the account, the account takes no second binding, and a manifest naming the account applies and exports without drift.

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

type accountsRow struct {
	Metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		AccountIDs []string `json:"accountIds"`
	} `json:"spec"`
}

func TestIntegration_ProviderAccountBindingCountsAcrossItsKeys(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tokenUpstreamResponse)
	}))
	t.Cleanup(upstream.Close)
	st := newStack(t)
	ctx := context.Background()
	userKey := st.seedHappyPath(upstream.URL, "sk-mock-upstream-key")
	f := &tokenFixture{stack: st}

	// A second system key at the happy path's host joins the first one's account.
	keys, err := st.stores.HostKey.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list host keys: %d, %v", len(keys), err)
	}
	first := keys[0]
	envName := uniqueEnvName("HK_")
	if err := os.Setenv(envName, "sk-mock-upstream-key-2"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(envName) })
	second := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "test-hostkey-2", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: first.Spec.HostID, AccountID: first.Spec.AccountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: envName}},
	}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, second), "hostkey")
	account := first.Spec.AccountID

	code, raw := st.adminDo(http.MethodPost, "/api/rate-limits", `{"metadata":{"name":"one-a-minute"},"spec":{"rules":[{"meter":"requests","amount":1,"window":60,"strategy":"fixed-window"}]}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/rate-limits = %d: %s", code, raw)
	}
	rl := decodeID(t, raw)
	code, raw = st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"key-cap"},"spec":{"rateLimits":[{"rateLimitId":"`+rl+`"}]}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	pol := decodeID(t, raw)
	binding := func(name string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"system"}},"spec":{"policyId":"` + pol + `","accountIds":["` + account + `"]}}`
	}

	code, raw = st.adminDo(http.MethodPost, "/api/policy-bindings", binding("key-cap-on-account"))
	if code != http.StatusCreated {
		t.Fatalf("POST binding on the provider account = %d: %s", code, raw)
	}
	var row accountsRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if !slices.Equal(row.Spec.AccountIDs, []string{account}) {
		t.Fatalf("accountIds = %v, want the keys' account %s", row.Spec.AccountIDs, account)
	}
	code, raw = st.adminDo(http.MethodPost, "/api/policy-bindings", binding("key-cap-again"))
	if code != http.StatusConflict || !strings.Contains(string(raw), "key-cap-on-account") {
		t.Fatalf("a second binding on the account = %d, want 409 naming key-cap-on-account: %s", code, raw)
	}

	if err := st.reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, reason := f.chat(userKey, "test-model"); got != http.StatusOK {
		t.Fatalf("first request = %d (%s), want 200", got, reason)
	}
	if got, reason := f.chat(userKey, "test-model"); got != http.StatusTooManyRequests {
		t.Fatalf("second request = %d (%s), want 429: the limit counts on the account, so the other key has no room either", got, reason)
	}
}

// A manifest binding a provider account by name applies, re-applies unchanged, and exports as the same account name.
func TestIntegration_ProviderAccountBindingManifestRoundTrip(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	mustUpsert(t, st.stores.Host.Upsert(ctx, &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "roundtrip-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}), "host")
	envName := uniqueEnvName("HK_")
	if err := os.Setenv(envName, "sk-roundtrip"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(envName) })
	const head = "apiVersion: relay.wyolet.dev/v1alpha3\n"
	bundle := head + `kind: Team
metadata: {name: roundtrip}
spec: {}
---
` + head + `kind: Account
metadata: {name: roundtrip-org, owner: {kind: team, name: roundtrip}}
spec: {host: roundtrip-host}
---
` + head + `kind: HostKey
metadata: {name: roundtrip-key, owner: {kind: team, name: roundtrip}}
spec: {hostId: roundtrip-host, account: roundtrip-org, valueFrom: {kind: env, env: ` + envName + `}}
---
` + head + `kind: RateLimit
metadata: {name: roundtrip-rpm, owner: {kind: team, name: roundtrip}}
spec: {rules: [{meter: requests, amount: 60, window: 1m, strategy: fixed-window}]}
---
` + head + `kind: Policy
metadata: {name: roundtrip-cap, owner: {kind: team, name: roundtrip}}
spec: {rateLimits: [{ref: roundtrip-rpm}]}
---
` + head + `kind: PolicyBinding
metadata: {name: roundtrip-cap-on-org, owner: {kind: team, name: roundtrip}}
spec: {policy: roundtrip-cap, accounts: [roundtrip-org]}
`
	for i, want := range []string{"create", "unchanged"} {
		code, plan, raw := st.applyBundle(bundle, "")
		if code != http.StatusOK || !plan.Applied || plan.action("PolicyBinding", "roundtrip-cap-on-org") != want {
			t.Fatalf("apply %d = %d, binding %q, want %q: %s", i+1, code, plan.action("PolicyBinding", "roundtrip-cap-on-org"), want, raw)
		}
	}
	code, raw := st.adminDo(http.MethodGet, "/api/policy-bindings/roundtrip-cap-on-org", "")
	if code != http.StatusOK {
		t.Fatalf("GET binding = %d: %s", code, raw)
	}
	var row accountsRow
	if err := json.Unmarshal(raw, &row); err != nil || len(row.Spec.AccountIDs) != 1 {
		t.Fatalf("stored binding = %s, want one account", raw)
	}

	code, exported := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, exported)
	}
	docs, err := manifest.Parse(bytes.NewReader(exported))
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	found := false
	for _, d := range docs {
		if pb := d.PolicyBinding; pb != nil && pb.Metadata.Name == "roundtrip-cap-on-org" {
			found = slices.Equal(pb.Spec.Accounts, []string{"roundtrip-org"})
		}
	}
	if !found {
		t.Fatalf("export does not bind the account by name:\n%s", exported)
	}
	code, plan, raw := st.applyBundle(string(exported), "dryRun=true")
	if code != http.StatusOK || plan.action("PolicyBinding", "roundtrip-cap-on-org") != "unchanged" {
		t.Fatalf("exported bundle = %d, binding %q, want unchanged: %s", code, plan.action("PolicyBinding", "roundtrip-cap-on-org"), raw)
	}
}
