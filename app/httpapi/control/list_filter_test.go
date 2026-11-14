package control

import (
	"context"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
)

// The list operations take an empty typed input, so the filter surface reaches the OpenAPI document, and the client generated from it, only through filterParams: one query parameter per accepted key, a repeatable one as an exploded array, an enum carried through.
func TestFilterParamsDescribeTheSchema(t *testing.T) {
	type param struct {
		Type, Items string
		Explode     bool
		Enum        []any
	}
	got := map[string]param{}
	for _, p := range filterParams(&policyFilter) {
		if p.In != "query" || p.Description == "" {
			t.Errorf("%s: in %q, description %q; want a described query parameter", p.Name, p.In, p.Description)
		}
		d := param{Type: p.Schema.Type, Enum: p.Schema.Enum, Explode: p.Explode != nil && *p.Explode}
		if p.Schema.Items != nil {
			d.Items, d.Enum = p.Schema.Items.Type, p.Schema.Items.Enum
		}
		got[p.Name] = d
	}
	str, many := param{Type: "string"}, param{Type: "array", Items: "string", Explode: true}
	want := map[string]param{
		"name": str, "owner": str, "enabled": {Type: "boolean"},
		"owner_id": many, "pool_id": many, "rate_limit_id": many, "budget_id": many, "label": many,
		"created_from": str, "created_to": str, "updated_from": str, "updated_to": str,
		"q": str, "limit": {Type: "integer"}, "offset": {Type: "integer"},
		"sort": {Type: "string", Enum: []any{"name", "-name", "created", "-created", "updated", "-updated"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("policy list parameters (-want +got):\n%s", diff)
	}
}

// huma skips unexported fields, so the filter input embedded in the logs query must stay exported or the route would bind and document no filter at all.
func TestUsageFilterParamsReachOpenAPI(t *testing.T) {
	api := humachi.New(chi.NewMux(), huma.DefaultConfig("t", "1.0.0"))
	huma.Register(api, huma.Operation{OperationID: "logs_list", Method: "GET", Path: "/logs"},
		func(ctx context.Context, _ *logsListInput) (*logsListOutput, error) { return nil, nil })

	have := map[string]bool{}
	for _, p := range api.OpenAPI().Paths["/logs"].Get.Parameters {
		have[p.Name] = p.In == "query"
	}
	for _, name := range []string{"from", "to", "model_id", "host_id", "policy_id", "status_class", "duration_ms_min", "ttft_ms_min", "host_key_id", "q", "tag", "cursor", "limit", "count"} {
		if !have[name] {
			t.Errorf("GET /logs lacks query parameter %s", name)
		}
	}
}
