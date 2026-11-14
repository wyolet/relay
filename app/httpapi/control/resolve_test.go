package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCatalogResolve(t *testing.T) {
	r, api := testAPI(t, scopeActors)
	registerResolve(api, Deps{Catalog: resolveCatalog().Load(t)}, nil)
	gpt4oCloud, gpt4oEdge, legacyCloud := resolveBindingRef{ModelID: "M2", HostID: "H1"}, resolveBindingRef{ModelID: "M2", HostID: "H2"}, resolveBindingRef{ModelID: "M3", HostID: "H1"}
	gpt4o := refResult{Ref: "openai/gpt-4o", Expanded: []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge"}, ModelIDs: []string{"M2"}, HostIDs: []string{"H1", "H2"}, Bindings: []resolveBindingRef{gpt4oCloud, gpt4oEdge}}
	dead := func(ref string) refResult {
		return refResult{Ref: ref, Expanded: []string{}, ModelIDs: []string{}, HostIDs: []string{}, Bindings: []resolveBindingRef{}}
	}
	provider := gpt4o
	provider.Ref = "openai"

	// Dead references are listed and add nothing; the union lists each model, host and binding once.
	var want, got resolveOutput
	want.Body.Refs = []string{"openai/legacy", "openai/gpt-4o", "acme/old", "openai"}
	want.Body.Results = []refResult{dead("openai/legacy"), gpt4o, dead("acme/old"), provider}
	want.Body.Unresolved = []string{"openai/legacy", "acme/old"}
	want.Body.Models = []resolveEntity{{ID: "M2", Name: "gpt-4o", DisplayName: "GPT-4o"}}
	want.Body.Hosts = []resolveEntity{{ID: "H1", Name: "cloud", DisplayName: "Cloud"}, {ID: "H2", Name: "edge"}}
	want.Body.Bindings = []resolveBindingRef{gpt4oCloud, gpt4oEdge}
	want.Body.Expanded = gpt4o.Expanded
	wantJSON(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve?ref=openai/legacy&ref=openai/gpt-4o&ref=acme/old&ref=openai", ""), http.StatusOK, &got.Body)
	if diff := cmp.Diff(want.Body, got.Body); diff != "" {
		t.Errorf("default (-want +got):\n%s", diff)
	}

	// The union keeps the order the references reach a row in; expanded is sorted.
	want.Body.Refs = []string{"openai/legacy", "openai"}
	want.Body.Results = []refResult{
		{Ref: "openai/legacy", Expanded: []string{"openai/legacy@cloud"}, ModelIDs: []string{"M3"}, HostIDs: []string{"H1"}, Bindings: []resolveBindingRef{legacyCloud}},
		{Ref: "openai", Expanded: []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge", "openai/legacy@cloud"}, ModelIDs: []string{"M2", "M3"}, HostIDs: []string{"H1", "H2"}, Bindings: []resolveBindingRef{gpt4oCloud, gpt4oEdge, legacyCloud}},
	}
	want.Body.Unresolved = []string{}
	want.Body.Models = []resolveEntity{{ID: "M3", Name: "legacy", Deprecated: "deprecated"}, {ID: "M2", Name: "gpt-4o", DisplayName: "GPT-4o"}}
	want.Body.Bindings = []resolveBindingRef{legacyCloud, gpt4oCloud, gpt4oEdge}
	want.Body.Expanded = []string{"openai/gpt-4o@cloud", "openai/gpt-4o@edge", "openai/legacy@cloud"}
	got = resolveOutput{}
	wantJSON(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve?ref=openai/legacy&ref=openai&includeDeprecated=true", ""), http.StatusOK, &got.Body)
	if diff := cmp.Diff(want.Body, got.Body); diff != "" {
		t.Errorf("includeDeprecated (-want +got):\n%s", diff)
	}
}

func TestCatalogResolveRefusals(t *testing.T) {
	r, api := testAPI(t, scopeActors)
	registerResolve(api, Deps{Catalog: resolveCatalog().Load(t)}, nil)
	wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve?ref=openai&ref=openai/", ""), http.StatusBadRequest, "", `modelref "openai/": trailing / requires a model`)
	wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve?ref=openai&ref=", ""), http.StatusBadRequest, "", "ref is required")
	wantFieldError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve", ""), "query.ref", "required query parameter is missing")

	r, api = testAPI(t, scopeActors)
	registerResolve(api, Deps{}, nil)
	wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/resolve?ref=openai", ""), http.StatusInternalServerError, "internal_error", "catalog not ready")
}
