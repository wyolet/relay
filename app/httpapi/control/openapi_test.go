package control

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// The UI's client is generated from this document, so it must describe every operation and field, and must not offer fields the policy model retired.
func TestControlOpenAPIIsDocumented(t *testing.T) {
	doc := controlDoc(t)
	for _, p := range openapitest.Problems(doc) {
		t.Error(p)
	}
}

func TestControlOpenAPIHasNoRetiredFields(t *testing.T) {
	doc := controlDoc(t)

	// Names no schema may carry at all.
	for _, name := range []string{"hostKeyIds", "rlBindings", "keySelection", "defaultPolicy", "allowMissingPolicy", "priority"} {
		if at := openapitest.PropertyPaths(doc, name); len(at) > 0 {
			t.Errorf("retired field %q appears at %v", name, at)
		}
	}
	// Names that survive elsewhere with another meaning, but not on these schemas.
	retiredOn := map[string][]string{
		"PolicySpec":         {"rateLimitId", "modelIds", "policyId", "credentialIds", "budgetIds", "includeDeprecated", "payloadLoggingEnabled"},
		"PolicyBindingSpec":  {"subjects", "projectId"},
		"HostKeySpec":        {"policyId"},
		"KeySpec":            {"policyId"},
		"ServiceAccountSpec": {"policyId"},
		"BindingSpec":        {"priority"},
		"HostSpec":           {"policies", "policyIds"},
		"TeamSpec":           {"budget", "budgetId", "budgetIds"},
		"ProjectSpec":        {"budget", "budgetId", "budgetIds"},
	}
	for schema, fields := range retiredOn {
		props := schemaProps(t, doc, schema)
		for _, f := range fields {
			if _, ok := props[f]; ok {
				t.Errorf("%s still has retired field %q", schema, f)
			}
		}
	}

	kinds := enumOf(t, doc, "AccountSpec", "kind")
	for _, k := range []string{"team", "project", "personal", "member", "service", "provider"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("AccountSpec.kind lacks %q: %v", k, kinds)
		}
	}
}

func TestControlOpenAPIHasPolicyModel(t *testing.T) {
	doc := controlDoc(t)
	paths, _ := doc["paths"].(map[string]any)
	for _, kind := range []string{"accounts", "pools", "budgets"} {
		for _, p := range []string{"", "/{ref}", "/by-id/{id}", "/by-id/{id}/references", "/by-id/{id}/detach", "/{ref}/keys"} {
			if kind == "budgets" && p == "/{ref}/keys" {
				continue
			}
			if _, ok := paths["/"+kind+p]; !ok {
				t.Errorf("no path /%s%s", kind, p)
			}
		}
	}
	for _, p := range []string{"/policy-bindings", "/policy-bindings/by-id/{id}", "/policies/{ref}/models", "/hosts/{ref}/policies"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("no path %s", p)
		}
	}

	wantProps := map[string][]string{
		"PolicySpec":          {"models", "poolIds", "rateLimits", "budgets"},
		"PolicyRateLimitLink": {"rateLimitId", "models", "shared"},
		"PolicyBudgetLink":    {"budgetId", "shared"},
		"PolicyBindingSpec":   {"policyId", "accountIds"},
		"HostKeySpec":         {"hostId", "accountId"},
		"KeySpec":             {"principal", "accountId"},
		"AccountSpec":         {"kind", "holder", "hostId", "orgId", "default", "payloadLogging", "closed"},
		"PoolSpec":            {"hostId", "credentialIds", "selection"},
		"BudgetSpec":          {"amountUsd", "period", "mode"},
	}
	for schema, fields := range wantProps {
		props := schemaProps(t, doc, schema)
		for _, f := range fields {
			if _, ok := props[f]; !ok {
				t.Errorf("%s lacks %q", schema, f)
			}
		}
	}
	for schema, prop := range map[string]string{"PoolSpec": "selection", "BudgetSpec": "period", "Owner": "kind"} {
		if len(enumOf(t, doc, schema, prop)) == 0 {
			t.Errorf("%s.%s declares no enum", schema, prop)
		}
	}

	// Deletes answer 200 with what they detached, or 409 with what blocks them.
	del := operation(t, doc, "/pools/by-id/{id}", "delete")
	if ref := responseRef(del, "200"); !strings.HasSuffix(ref, "/deleteDetachedBody") {
		t.Errorf("delete 200 schema = %q", ref)
	}
	if ref := responseRef(del, "409"); !strings.HasSuffix(ref, "/conflictBody") {
		t.Errorf("delete 409 schema = %q", ref)
	}
	if _, ok := schemaProps(t, doc, "conflictBody")["blockers"]; !ok {
		t.Error("conflictBody has no blockers")
	}
}

// Usage and log records carry the lane account and the refusal class, the error envelope the class, and both list endpoints filter on them.
func TestControlOpenAPIHasRefusalAndLane(t *testing.T) {
	doc := controlDoc(t)
	want := []string{"model_not_allowed", "host_not_reachable", "no_capacity"}
	for _, schema := range []string{"Event", "APIErrorBody"} {
		if got := enumOf(t, doc, schema, "refusal"); !slices.Equal(got, want) {
			t.Errorf("%s.refusal enum = %v, want %v", schema, got, want)
		}
	}
	if _, ok := schemaProps(t, doc, "Event")["lane_account_id"]; !ok {
		t.Error("Event lacks lane_account_id")
	}
	for _, path := range []string{"/usage/events", "/logs"} {
		params, _ := operation(t, doc, path, "get")["parameters"].([]any)
		found := map[string]bool{}
		for _, p := range params {
			if name, _ := p.(map[string]any)["name"].(string); name != "" {
				found[name] = true
			}
		}
		for _, name := range []string{"refusal", "lane_account_id"} {
			if !found[name] {
				t.Errorf("GET %s does not filter on %s", path, name)
			}
		}
	}
}

func controlDoc(t *testing.T) map[string]any {
	t.Helper()
	api := Mount(chi.NewRouter(), mountDeps(t))
	doc, err := openapitest.Decode(api.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func schemaProps(t *testing.T, doc map[string]any, name string) map[string]any {
	t.Helper()
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	s, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("no schema %s", name)
	}
	props, _ := s["properties"].(map[string]any)
	return props
}

func enumOf(t *testing.T, doc map[string]any, schema, prop string) []string {
	t.Helper()
	p, _ := schemaProps(t, doc, schema)[prop].(map[string]any)
	vals, _ := p["enum"].([]any)
	var out []string
	for _, v := range vals {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func operation(t *testing.T, doc map[string]any, path, method string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("no %s %s", method, path)
	}
	return op
}

func responseRef(op map[string]any, code string) string {
	r, _ := op["responses"].(map[string]any)[code].(map[string]any)
	c, _ := r["content"].(map[string]any)
	j, _ := c["application/json"].(map[string]any)
	s, _ := j["schema"].(map[string]any)
	ref, _ := s["$ref"].(string)
	return ref
}
