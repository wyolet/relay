package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// Each pseudo kind is absent from the document and answers 404 unless RELAY_PSEUDO_KINDS names it, alone or as all; a served one is documented without problems and answers its warnings.
func TestPseudoKinds_EachAloneAllAndNone(t *testing.T) {
	paths := map[string]string{"agents": "/pseudo-agents", "access": "/pseudo-access", "limits": "/pseudo-limits", "host-keys": "/pseudo-host-keys"}
	cases := map[string][]string{"none": nil, "all": {"all"}}
	for kind := range paths {
		cases[kind+" alone"] = []string{kind}
	}
	for name, kinds := range cases {
		t.Run(name, func(t *testing.T) {
			d := mountDeps(t)
			d.PseudoKinds = kinds
			r := chi.NewRouter()
			api := Mount(r, d)
			doc, err := openapitest.Decode(api.OpenAPI())
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range openapitest.Problems(doc) {
				t.Error(p)
			}
			documented, _ := doc["paths"].(map[string]any)
			for kind, path := range paths {
				served := name == "all" || name == kind+" alone"
				_, inDoc := documented[path]
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
				want := http.StatusNotFound
				if served {
					want = http.StatusUnauthorized
				}
				if inDoc != served || rec.Code != want {
					t.Errorf("%s: documented %v, POST %d; want %v, %d", path, inDoc, rec.Code, served, want)
				}
				if served && !answersWarnings(t, doc, path) {
					t.Errorf("%s: the 201 answer declares no warnings", path)
				}
			}
		})
	}
}

// answersWarnings reports whether the 201 answer of the POST at path declares a required warnings list.
func answersWarnings(t *testing.T, doc map[string]any, path string) bool {
	t.Helper()
	ref := responseRef(operation(t, doc, path, "post"), "201")
	schemas, _ := doc["components"].(map[string]any)["schemas"].(map[string]any)
	s, _ := schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any)
	props, _ := s["properties"].(map[string]any)
	required, _ := s["required"].([]any)
	_, has := props["warnings"]
	for _, r := range required {
		if r == "warnings" {
			return has
		}
	}
	return false
}
