//go:build integration

package integration_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// linkedCatalog is the catalog part both stacks share: export leaves catalog rows out, so the second stack gets them the same way.
const linkedCatalog = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Provider
metadata:
  name: rt-prov
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata:
  name: rt-host
spec:
  baseURL: https://rt.invalid
`

// linkedBundle exercises every link a v1alpha3 document can carry: pools (with order), credentials, accounts, rate limits with and without a model scope, budgets, and bindings on a caller and an upstream rung.
func linkedBundle(envA, envB string) string {
	return linkedCatalog + `---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Team
metadata:
  name: rt-team
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Project
metadata:
  name: rt-project
spec:
  team: rt-team
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: ServiceAccount
metadata:
  name: rt-sa
spec:
  project: rt-project
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata:
  name: rt-org
  owner: {kind: project, name: rt-project}
spec:
  host: rt-host
  orgId: org-rt
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostKey
metadata:
  name: rt-key-a
  owner: {kind: project, name: rt-project}
spec:
  hostId: rt-host
  account: rt-org
  valueFrom: {kind: env, env: ` + envA + `}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostKey
metadata:
  name: rt-key-b
  owner: {kind: project, name: rt-project}
spec:
  hostId: rt-host
  account: rt-org
  valueFrom: {kind: env, env: ` + envB + `}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Pool
metadata:
  name: rt-pool
  owner: {kind: project, name: rt-project}
spec:
  host: rt-host
  credentials: [rt-key-b, rt-key-a]
  selection: round-robin
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Budget
metadata:
  name: rt-budget
  owner: {kind: project, name: rt-project}
spec:
  amountUsd: "25.50"
  period: week
  mode: alert
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RateLimit
metadata:
  name: rt-limit
  owner: {kind: project, name: rt-project}
spec:
  rules:
    - {meter: requests, amount: 60, window: 1m, strategy: fixed-window}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Policy
metadata:
  name: rt-caller
  owner: {kind: project, name: rt-project}
spec:
  includeDeprecated: true
  pools: [rt-pool]
  credentials: [rt-key-a]
  models: [rt-prov]
  rateLimits:
    - ref: rt-limit
  budgets: [rt-budget]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Policy
metadata:
  name: rt-tier
  owner: {kind: project, name: rt-project}
spec:
  models: [rt-prov]
  rateLimits:
    - ref: rt-limit
      models: [rt-prov/rt-model]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata:
  name: rt-caller-binding
  owner: {kind: project, name: rt-project}
spec:
  policy: rt-caller
  subjects:
    - {kind: serviceaccount, name: rt-sa}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata:
  name: rt-tier-binding
  owner: {kind: project, name: rt-project}
spec:
  policy: rt-tier
  subjects:
    - {kind: account, name: rt-org}
`
}

// linkKinds are the kinds whose specs carry links.
var linkKinds = map[string]bool{"Account": true, "HostKey": true, "Pool": true, "Budget": true, "RateLimit": true, "Policy": true, "PolicyBinding": true}

// exportedSpecs returns each linking document's spec and owner by kind/name, from a stack's export.
func exportedSpecs(t *testing.T, st *stack) (map[string]any, string) {
	t.Helper()
	code, body := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export = %d %s", code, body)
	}
	out := map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name  string         `yaml:"name"`
				Owner map[string]any `yaml:"owner"`
			} `yaml:"metadata"`
			Spec map[string]any `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode export: %v\n%s", err, body)
		}
		if linkKinds[doc.Kind] && strings.HasPrefix(doc.Metadata.Name, "rt-") {
			out[doc.Kind+"/"+doc.Metadata.Name] = map[string]any{"owner": doc.Metadata.Owner, "spec": doc.Spec}
		}
	}
	return out, string(body)
}

// An export applies back as a no-op, and applied to a fresh relay it recreates every link: the second relay exports the same specs.
func TestIntegration_ManifestExportApplyKeepsEveryLink(t *testing.T) {
	t.Parallel()
	envA, envB := uniqueEnvName("RT_A_"), uniqueEnvName("RT_B_")
	for _, e := range []string{envA, envB} {
		if err := os.Setenv(e, "sk-"+e); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Unsetenv(e) })
	}

	first := newStack(t)
	if code, _, raw := first.applyBundle(linkedBundle(envA, envB), ""); code != http.StatusOK {
		t.Fatalf("apply bundle = %d %s", code, raw)
	}
	want, text := exportedSpecs(t, first)
	if len(want) != 10 {
		t.Fatalf("export holds %d linking documents, want 10:\n%s", len(want), text)
	}
	field := func(doc, name string) any {
		return want[doc].(map[string]any)["spec"].(map[string]any)[name]
	}
	for _, tc := range []struct {
		doc, field string
		want       any
	}{
		{"Policy/rt-caller", "pools", []any{"rt-pool"}},
		{"Policy/rt-caller", "credentials", []any{"rt-key-a"}},
		{"Policy/rt-caller", "budgets", []any{"rt-budget"}},
		{"Policy/rt-caller", "models", []any{"rt-prov"}},
		{"Policy/rt-caller", "includeDeprecated", true},
		{"Policy/rt-caller", "rateLimits", []any{map[string]any{"ref": "rt-limit"}}},
		{"Policy/rt-tier", "rateLimits", []any{map[string]any{"ref": "rt-limit", "models": []any{"rt-prov/rt-model"}}}},
		{"Pool/rt-pool", "credentials", []any{"rt-key-b", "rt-key-a"}},
		{"Pool/rt-pool", "selection", "round-robin"},
		{"HostKey/rt-key-a", "account", "rt-org"},
		{"Budget/rt-budget", "period", "week"},
		{"Budget/rt-budget", "mode", "alert"},
		{"PolicyBinding/rt-caller-binding", "subjects", []any{map[string]any{"kind": "serviceaccount", "name": "rt-sa"}}},
		{"PolicyBinding/rt-tier-binding", "subjects", []any{map[string]any{"kind": "account", "name": "rt-org"}}},
	} {
		if got := field(tc.doc, tc.field); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("exported %s spec.%s = %#v, want %#v", tc.doc, tc.field, got, tc.want)
		}
	}

	_, again, raw := first.applyBundle(text, "dryRun=true")
	for _, e := range again.Plan {
		if e.Action != "unchanged" {
			t.Fatalf("re-applying the export reports %s on %s/%s (%v)\n%s", e.Action, e.Kind, e.Name, e.ChangedFields, raw)
		}
	}

	second := newStack(t)
	if code, _, raw := second.applyBundle(linkedCatalog, ""); code != http.StatusOK {
		t.Fatalf("apply catalog to the second relay = %d %s", code, raw)
	}
	if code, _, raw := second.applyBundle(text, ""); code != http.StatusOK {
		t.Fatalf("apply the export to the second relay = %d %s", code, raw)
	}
	got, _ := exportedSpecs(t, second)
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s after the round trip:\n got  %v\n want %v", k, got[k], w)
		}
	}
}
