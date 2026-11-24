package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// The access and limits routes, like every pseudo kind, are absent from the document and answer 404 unless RELAY_PSEUDO_KINDS names them.
func TestPseudoKinds_AccessAndLimitsGated(t *testing.T) {
	for _, tc := range []struct {
		kinds  []string
		served map[string]bool
	}{
		{nil, map[string]bool{}},
		{[]string{"agents"}, map[string]bool{}},
		{[]string{"access"}, map[string]bool{"/pseudo-access": true}},
		{[]string{"limits"}, map[string]bool{"/pseudo-limits": true}},
		{[]string{"all"}, map[string]bool{"/pseudo-access": true, "/pseudo-limits": true}},
	} {
		d := mountDeps(t)
		d.PseudoKinds = tc.kinds
		r := chi.NewRouter()
		api := Mount(r, d)
		doc, err := openapitest.Decode(api.OpenAPI())
		if err != nil {
			t.Fatal(err)
		}
		paths, _ := doc["paths"].(map[string]any)
		for _, path := range []string{"/pseudo-access", "/pseudo-limits"} {
			_, documented := paths[path]
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			want := http.StatusNotFound
			if tc.served[path] {
				want = http.StatusUnauthorized
			}
			if documented != tc.served[path] || rec.Code != want {
				t.Errorf("kinds %v, %s: documented %v, POST %d; want %v, %d", tc.kinds, path, documented, rec.Code, tc.served[path], want)
			}
		}
		if tc.served["/pseudo-access"] {
			for _, p := range openapitest.Problems(doc) {
				t.Error(p)
			}
		}
	}
}
