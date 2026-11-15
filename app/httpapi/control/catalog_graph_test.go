package control

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/pkg/catalogkind/model"
)

func TestCatalogGraph(t *testing.T) {
	r, api := testAPI(t, scopeActors)
	registerCatalogGraph(api, Deps{Catalog: resolveCatalog().Load(t)}, nil)
	openai := []graphBinding{{HostID: "H1", Adapter: "openai"}}
	gpt35 := graphModel{ID: "M1", Name: "gpt-3-5", ProviderID: "P1", Pointer: "gpt-3-5-snap"}
	gpt4o := graphModel{ID: "M2", Name: "gpt-4o", DisplayName: "GPT-4o", ProviderID: "P1", Featured: true, Capabilities: model.Capabilities{Tools: true},
		ContextWindowTotal: 128000, ContextWindowInput: 100000, Pointer: "gpt-4o-snap", Bindings: append(openai[:1:1], graphBinding{HostID: "H2", Adapter: "openai"})}
	legacy := graphModel{ID: "M3", Name: "legacy", ProviderID: "P1", Deprecated: "deprecated", Pointer: "legacy-snap", Bindings: openai}

	for _, tc := range []struct {
		name, query string
		want        []graphModel
	}{
		{"enabled models, deprecated left out", "", []graphModel{gpt35, gpt4o}},
		{"deprecated models flagged", "?includeDeprecated=true", []graphModel{gpt35, gpt4o, legacy}},
		{"every label must match", "?label=featured=true&label=tier=flagship", []graphModel{gpt4o}},
		{"one label missing", "?label=featured=true&label=tier=budget", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want, got graphOutput
			want.Body.Providers = []graphProvider{{ID: "P2", Name: "acme"}, {ID: "P1", Name: "openai", DisplayName: "OpenAI"}}
			want.Body.Hosts = []graphHost{{ID: "H1", Name: "cloud", DisplayName: "Cloud", IconPath: "icons/cloud.svg"}, {ID: "H2", Name: "edge"}}
			want.Body.Models = tc.want
			w := scopeReq(t, r, "alice", http.MethodGet, "/catalog/graph"+tc.query, "")
			wantJSON(t, w, http.StatusOK, &got.Body)
			if diff := cmp.Diff(want.Body, got.Body, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("graph (-want +got):\n%s", diff)
			}
			if got.Body.Models == nil || slices.ContainsFunc(got.Body.Models, func(m graphModel) bool { return m.Bindings == nil }) {
				t.Errorf("models and their bindings must be arrays, never null: %s", w.Body)
			}
		})
	}

	wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/graph?label=featured", ""), http.StatusBadRequest, "", "label: must be key=value")
	r, api = testAPI(t, scopeActors)
	registerCatalogGraph(api, Deps{}, nil)
	wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/catalog/graph", ""), http.StatusInternalServerError, "internal_error", "catalog not ready")
}

// A snapshot can still hold a disabled binding of an enabled model; the graph offers only hosts that serve.
func TestGraphModels_PrunesDisabledBindings(t *testing.T) {
	idx := resolveIndexes(t)["disabled rows indexed"]
	if n := len(idx.snap.BindingsForModel("M1")); n != 1 {
		t.Fatalf("the snapshot holds %d bindings of gpt-3-5, want its disabled one", n)
	}
	if got := graphModels(idx, false, nil)[0]; got.Name != "gpt-3-5" || got.Bindings == nil || len(got.Bindings) != 0 {
		t.Fatalf("model = %+v, want gpt-3-5 with no bindings", got)
	}
}

func TestParseLabelSelector(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     []string
		want    map[string]string
		wantErr bool
	}{
		{name: "none"},
		{name: "pairs", raw: []string{"featured=true", "tier=flagship"}, want: map[string]string{"featured": "true", "tier": "flagship"}},
		{name: "value keeps later equals signs", raw: []string{"a=b=c"}, want: map[string]string{"a": "b=c"}},
		{name: "empty value", raw: []string{"a="}, want: map[string]string{"a": ""}},
		{name: "no equals sign", raw: []string{"a=b", "nokey"}, wantErr: true},
		{name: "empty key", raw: []string{"=v"}, wantErr: true},
	} {
		got, err := parseLabelSelector(tc.raw)
		if tc.wantErr != (err != nil) || (err != nil && err.Error() != "label: must be key=value") {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestDeprecationStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec model.Spec
		want string
	}{
		{"no marker", model.Spec{}, ""},
		{"a date alone", model.Spec{DeprecationDate: "2025-01-01"}, "deprecated"},
		{"a status wins over the date", model.Spec{DeprecationDate: "2025-01-01", Deprecation: &model.Deprecation{Status: model.DeprecationSunset}}, "sunset"},
		{"an empty status falls back to the date", model.Spec{DeprecationDate: "2025-01-01", Deprecation: &model.Deprecation{}}, "deprecated"},
		{"an empty status and no date", model.Spec{Deprecation: &model.Deprecation{}}, ""},
	} {
		if got := deprecationStatus(&model.Model{Spec: tc.spec}); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
