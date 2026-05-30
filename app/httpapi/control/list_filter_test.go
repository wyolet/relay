package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/filter"
)

// TestFilterParamsReachOpenAPI proves the schema-derived filter params land
// in the generated OpenAPI document — the contract the frontend's TS codegen
// consumes. Without this wiring the list ops (empty typed input) would expose
// zero query params.
func TestFilterParamsReachOpenAPI(t *testing.T) {
	api := humachi.New(chi.NewMux(), huma.DefaultConfig("t", "1.0.0"))
	huma.Register(api, huma.Operation{
		OperationID: "list_policies",
		Method:      "GET",
		Path:        "/policies",
		Parameters:  filterParams(&policyFilter),
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Items []string `json:"items"`
		}
	}, error) {
		return nil, nil
	})

	spec, err := json.Marshal(api.OpenAPI())
	if err != nil {
		t.Fatalf("marshal openapi: %v", err)
	}
	doc := string(spec)
	// A representative spread: a bool, a repeatable IN, a time range pair,
	// and the engine-owned sort/q/label/limit params.
	for _, want := range []string{
		`"name":"enabled"`, `"name":"payload_logging"`, `"name":"model_id"`,
		`"name":"created_from"`, `"name":"created_to"`, `"name":"owner"`,
		`"name":"q"`, `"name":"label"`, `"name":"sort"`, `"name":"limit"`, `"name":"offset"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("openapi missing param %s", want)
		}
	}
	// model_id is repeatable → array-typed (explode).
	if !strings.Contains(doc, `"type":"array"`) {
		t.Error("expected an array-typed (repeatable) param in the spec")
	}
}

func TestSchemaParamsExpansion(t *testing.T) {
	ps := policyFilter.Params()
	byName := map[string]filter.Param{}
	for _, p := range ps {
		byName[p.Name] = p
	}
	// Time field "created" expands to a _from/_to pair.
	if _, ok := byName["created_from"]; !ok {
		t.Error("created_from missing")
	}
	if _, ok := byName["created_to"]; !ok {
		t.Error("created_to missing")
	}
	if p := byName["model_id"]; !p.Repeatable {
		t.Error("model_id should be repeatable")
	}
	if p := byName["enabled"]; p.Type != "boolean" {
		t.Errorf("enabled type = %q, want boolean", p.Type)
	}
	// sort enum lists the sortable fields.
	if p := byName["sort"]; len(p.Enum) == 0 {
		t.Error("sort param should carry an enum of sortable fields")
	}
}
