package control

import (
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// The UI's client is generated from this document, so it must describe every operation and field, carry the policy model, sharing and refusal surface the UI reads, and not offer fields the policy model retired.
func TestControlOpenAPI(t *testing.T) {
	doc := controlDoc(t)
	paths, _ := doc["paths"].(map[string]any)

	t.Run("every operation and field is documented", func(t *testing.T) {
		for _, p := range openapitest.Problems(doc) {
			t.Error(p)
		}
	})

	t.Run("retired fields are gone", func(t *testing.T) {
		for _, name := range []string{"hostKeyIds", "rlBindings", "keySelection", "defaultPolicy", "allowMissingPolicy", "priority"} {
			if at := openapitest.PropertyPaths(doc, name); len(at) > 0 {
				t.Errorf("retired field %q appears at %v", name, at)
			}
		}
		// Names that survive elsewhere with another meaning, but not on these schemas.
		for schema, fields := range map[string][]string{
			"PolicySpec":         {"rateLimitId", "modelIds", "policyId", "credentialIds", "budgetIds", "includeDeprecated", "payloadLoggingEnabled"},
			"PolicyBindingSpec":  {"subjects", "projectId"},
			"HostKeySpec":        {"policyId"},
			"KeySpec":            {"policyId"},
			"ServiceAccountSpec": {"policyId"},
			"HostSpec":           {"policies", "policyIds"},
			"TeamSpec":           {"budget", "budgetId", "budgetIds"},
			"ProjectSpec":        {"budget", "budgetId", "budgetIds"},
		} {
			props := schemaProps(t, doc, schema)
			for _, f := range fields {
				if _, ok := props[f]; ok {
					t.Errorf("%s still has retired field %q", schema, f)
				}
			}
		}
	})

	t.Run("schemas carry the fields the UI reads", func(t *testing.T) {
		for schema, fields := range map[string][]string{
			"PolicySpec":          {"models", "poolIds", "rateLimits", "budgets"},
			"PolicyRateLimitLink": {"rateLimitId", "models", "shared"},
			"PolicyBudgetLink":    {"budgetId", "shared"},
			"PolicyBindingSpec":   {"policyId", "accountIds"},
			"HostKeySpec":         {"hostId", "accountId"},
			"HostKeyView":         {"readOnly", "prefix"},
			"KeySpec":             {"principal", "accountId"},
			"AccountSpec":         {"kind", "holder", "hostId", "orgId", "groupedByRelay", "payloadLogging", "closed", "sourceAccountId", "sharedBy"},
			"AccountShare":        {"sharedAccount", "receiver", "usage"},
			"ShareUsage":          {"requests", "costNanos", "unpriced"},
			"AccountShareParty":   {"kind", "id", "name"},
			"AccountReceiver":     {"kind", "id"},
			"AccountPolicy":       {"binding", "previousBindingId", "previousBindingDeleted"},
			"PoolSpec":            {"hostId", "accountIds", "credentialIds", "selection"},
			"BudgetSpec":          {"amountUsd", "period", "mode"},
			"Event":               {"lane_account_id", "via_account_id"},
			"conflictBody":        {"blockers"},
		} {
			props := schemaProps(t, doc, schema)
			for _, f := range fields {
				if _, ok := props[f]; !ok {
					t.Errorf("%s lacks %q", schema, f)
				}
			}
		}
	})

	t.Run("closed sets are enums", func(t *testing.T) {
		for _, tc := range []struct {
			schema, prop string
			want         []string
		}{
			{"AccountSpec", "kind", []string{"team", "project", "personal", "member", "service", "provider"}},
			{"Event", "refusal", []string{"model_not_allowed", "host_not_reachable", "no_capacity"}},
			{"APIErrorBody", "refusal", []string{"model_not_allowed", "host_not_reachable", "no_capacity"}},
			{"BudgetSpec", "period", []string{"day", "week", "month"}},
			{"BudgetSpec", "mode", []string{"hard", "alert"}},
		} {
			if got := enumOf(t, doc, tc.schema, tc.prop); !slices.Equal(got, tc.want) {
				t.Errorf("%s.%s enum = %v, want %v", tc.schema, tc.prop, got, tc.want)
			}
		}
		for schema, prop := range map[string]string{"PoolSpec": "selection", "Owner": "kind"} {
			if len(enumOf(t, doc, schema, prop)) == 0 {
				t.Errorf("%s.%s declares no enum", schema, prop)
			}
		}
	})

	t.Run("policy model routes", func(t *testing.T) {
		want := []string{"/policy-bindings", "/policy-bindings/by-id/{id}", "/policies/{ref}/models", "/hosts/{ref}/policies", "/accounts/{ref}/keys", "/pools/{ref}/keys"}
		for _, kind := range []string{"accounts", "pools", "budgets"} {
			for _, p := range []string{"", "/{ref}", "/by-id/{id}", "/by-id/{id}/references", "/by-id/{id}/detach"} {
				want = append(want, "/"+kind+p)
			}
		}
		for _, p := range want {
			if _, ok := paths[p]; !ok {
				t.Errorf("no path %s", p)
			}
		}
	})

	// Deletes answer 200 with what they detached, or 409 with what blocks them; the account pages read named schemas.
	t.Run("answers are named schemas", func(t *testing.T) {
		for _, tc := range []struct{ path, method, code, schema string }{
			{"/pools/by-id/{id}", "delete", "200", "deleteDetachedBody"},
			{"/pools/by-id/{id}", "delete", "409", "conflictBody"},
			{"/accounts/{ref}/shares", "post", "201", "Account"},
			{"/accounts/{ref}/shares", "get", "200", "AccountShares"},
			{"/accounts/{ref}/policy", "put", "200", "AccountPolicy"},
		} {
			if ref := responseRef(operation(t, doc, tc.path, tc.method), tc.code); ref != "#/components/schemas/"+tc.schema {
				t.Errorf("%s %s %s answers %q, want %s", tc.method, tc.path, tc.code, ref, tc.schema)
			}
		}
	})

	t.Run("usage and logs filter on lane, share and refusal", func(t *testing.T) {
		for _, path := range []string{"/usage/events", "/logs"} {
			params, _ := operation(t, doc, path, "get")["parameters"].([]any)
			var names []string
			for _, p := range params {
				name, _ := p.(map[string]any)["name"].(string)
				names = append(names, name)
			}
			for _, name := range []string{"refusal", "lane_account_id", "via_account_id"} {
				if !slices.Contains(names, name) {
					t.Errorf("GET %s does not filter on %s", path, name)
				}
			}
		}
	})
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
