//go:build integration

// credential_binding_test.go binds a policy to a host key and checks it lands on the key's account: the API answers with the account, the limit counts across every key of the account, the account takes no second binding, and a manifest naming the key applies and exports without drift.

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

type subjectsRow struct {
	Metadata struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Subjects []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"subjects"`
	} `json:"spec"`
}

func TestIntegration_CredentialBindingCountsOnTheAccount(t *testing.T) {
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

	// A second system key at the happy path's host joins the same default account as the first.
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
		Spec: hostkey.Spec{HostID: first.Spec.HostID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: envName}},
	}
	mustUpsert(t, st.stores.HostKey.Upsert(ctx, second), "hostkey")
	if second.Spec.AccountID != first.Spec.AccountID || first.Spec.AccountID == "" {
		t.Fatalf("accounts %q and %q, want one default account", first.Spec.AccountID, second.Spec.AccountID)
	}
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
	binding := func(name, subject string) string {
		return `{"metadata":{"name":"` + name + `","owner":{"kind":"system"}},"spec":{"policyId":"` + pol + `","subjects":[` + subject + `]}}`
	}

	code, raw = st.adminDo(http.MethodPost, "/api/policy-bindings", binding("key-cap-on-first", `{"kind":"credential","id":"`+first.Meta.ID+`"}`))
	if code != http.StatusCreated {
		t.Fatalf("POST binding on a host key = %d: %s", code, raw)
	}
	var row subjectsRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if s := row.Spec.Subjects; len(s) != 1 || s[0].Kind != "account" || s[0].ID != account {
		t.Fatalf("subjects = %+v, want the key's account %s", s, account)
	}

	for _, tc := range []struct{ name, subject string }{
		{"the account itself", `{"kind":"account","id":"` + account + `"}`},
		{"another key of the account", `{"kind":"credential","id":"` + second.Meta.ID + `"}`},
	} {
		t.Run("a second binding on "+tc.name, func(t *testing.T) {
			code, raw := st.adminDo(http.MethodPost, "/api/policy-bindings", binding("key-cap-again-"+ids.New()[:8], tc.subject))
			if code != http.StatusConflict || !strings.Contains(string(raw), "key-cap-on-first") {
				t.Fatalf("POST = %d, want 409 naming key-cap-on-first: %s", code, raw)
			}
		})
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

// A manifest binding a host key applies as the account binding the store keeps, re-applies unchanged, and exports as the account.
func TestIntegration_CredentialBindingManifestRoundTrip(t *testing.T) {
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
` + head + `kind: HostKey
metadata: {name: roundtrip-key, owner: {kind: team, name: roundtrip}}
spec: {hostId: roundtrip-host, valueFrom: {kind: env, env: ` + envName + `}}
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
metadata: {name: roundtrip-cap-on-key, owner: {kind: team, name: roundtrip}}
spec: {policy: roundtrip-cap, subjects: [{kind: credential, name: roundtrip-key}]}
`
	for i, want := range []string{"create", "unchanged"} {
		code, plan, raw := st.applyBundle(bundle, "")
		if code != http.StatusOK || !plan.Applied || plan.action("PolicyBinding", "roundtrip-cap-on-key") != want {
			t.Fatalf("apply %d = %d, binding %q, want %q: %s", i+1, code, plan.action("PolicyBinding", "roundtrip-cap-on-key"), want, raw)
		}
	}
	code, raw := st.adminDo(http.MethodGet, "/api/policy-bindings/roundtrip-cap-on-key", "")
	if code != http.StatusOK {
		t.Fatalf("GET binding = %d: %s", code, raw)
	}
	var row subjectsRow
	if err := json.Unmarshal(raw, &row); err != nil || len(row.Spec.Subjects) != 1 || row.Spec.Subjects[0].Kind != "account" {
		t.Fatalf("stored binding = %s, want one account subject", raw)
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
		if pb := d.PolicyBinding; pb != nil && pb.Metadata.Name == "roundtrip-cap-on-key" {
			found = len(pb.Spec.Subjects) == 1 && pb.Spec.Subjects[0].Kind == "account"
		}
	}
	if !found {
		t.Fatalf("export does not bind the account:\n%s", exported)
	}
	code, plan, raw := st.applyBundle(string(exported), "dryRun=true")
	if code != http.StatusOK || plan.action("PolicyBinding", "roundtrip-cap-on-key") != "unchanged" {
		t.Fatalf("exported bundle = %d, binding %q, want unchanged: %s", code, plan.action("PolicyBinding", "roundtrip-cap-on-key"), raw)
	}
}
