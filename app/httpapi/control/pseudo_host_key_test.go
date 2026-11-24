package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// POST /pseudo-host-keys exists only while RELAY_PSEUDO_KINDS names host-keys or all: otherwise it is absent from the document and answers 404.
func TestPseudoHostKeys_FlagGatesTheRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kinds  []string
		served bool
		status int
	}{
		{"off", nil, false, http.StatusNotFound},
		{"another kind only", []string{"agents"}, false, http.StatusNotFound},
		{"by name", []string{"host-keys"}, true, http.StatusUnauthorized},
		{"all", []string{"all"}, true, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := mountDeps(t)
			d.PseudoKinds = tc.kinds
			r := chi.NewRouter()
			api := Mount(r, d)
			doc, err := openapitest.Decode(api.OpenAPI())
			if err != nil {
				t.Fatal(err)
			}
			paths, _ := doc["paths"].(map[string]any)
			if _, ok := paths["/pseudo-host-keys"]; ok != tc.served {
				t.Errorf("/pseudo-host-keys in the document: %v, want %v", ok, tc.served)
			}
			if tc.served {
				for _, p := range openapitest.Problems(doc) {
					t.Error(p)
				}
				op := operation(t, doc, "/pseudo-host-keys", "post")
				for code, want := range map[string]string{"422": "#/components/schemas/pseudoRefusal", "409": "#/components/schemas/pseudoOrganizationRefusal"} {
					if ref := responseRef(op, code); ref != want {
						t.Errorf("%s answers %q, want %q", code, ref, want)
					}
				}
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pseudo-host-keys", strings.NewReader(`{}`)))
			if rec.Code != tc.status {
				t.Errorf("POST /pseudo-host-keys: %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
		})
	}
}
