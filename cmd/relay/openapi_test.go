package main

import (
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// The inference document is built from the adapter specs registered here, so its guard lives beside them: every operation and field described, and every refusal a caller can meet named on the generation routes.
func TestInferenceOpenAPIIsDocumented(t *testing.T) {
	doc := inferenceDoc(t)
	for _, p := range openapitest.Problems(doc) {
		t.Error(p)
	}

	paths, _ := doc["paths"].(map[string]any)
	refusals := map[string][]string{
		"403": {"model_not_allowed", "policy_disabled", "subject_disabled", "duplicate_binding", "unpriced_under_budget", "account_closed", "account_not_held"},
		"429": {"rate_limit_exceeded", "budget_exceeded"},
		"404": {"model_not_found"},
	}
	generation := 0
	for path, item := range paths {
		op, ok := item.(map[string]any)["post"].(map[string]any)
		if !ok {
			continue
		}
		generation++
		responses, _ := op["responses"].(map[string]any)
		for code, codes := range refusals {
			r, _ := responses[code].(map[string]any)
			desc, _ := r["description"].(string)
			for _, c := range codes {
				if !strings.Contains(desc, c) {
					t.Errorf("POST %s: %s does not name %s", path, code, c)
				}
			}
		}
		r429, _ := responses["429"].(map[string]any)
		headers, _ := r429["headers"].(map[string]any)
		if _, ok := headers["Retry-After"]; !ok {
			t.Errorf("POST %s: 429 does not declare Retry-After", path)
		}
		if _, ok := op["requestBody"]; !ok {
			t.Errorf("POST %s: no request body", path)
		}
	}
	if generation == 0 {
		t.Fatal("no generation routes in the document")
	}
}

func inferenceDoc(t *testing.T) map[string]any {
	t.Helper()
	reg := buildSpecRegistry(buildAdapterSpecs())
	api := inference.Mount(chi.NewRouter(), inference.Deps{
		Catalog:       &appcatalog.Catalog{},
		Specs:         reg,
		Adapters:      reg.AdapterMap(),
		Profiles:      buildClientProfiles(),
		RouteMounters: []inference.RouteMounter{inference.MountRegistry(reg)},
	})
	doc, err := openapitest.Decode(api.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
