package control

import (
	"net/http"
	"reflect"
	"testing"
)

// Generated clients send list filters as repeated params; each repeat must reach the query, not just the first.
func TestUsageListFiltersTakeRepeatedParams(t *testing.T) {
	reader := &fakeUsageReader{}
	h := newUsageHarness(t, testRBAC(), reader)
	for _, path := range []string{"/usage/summary", "/logs"} {
		w := scopeReq(t, h, "root", http.MethodGet, path+"?model_id=m1&model_id=m2&tag=env:prod&tag=team:a&status=429&status=503", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, w.Code, w.Body)
		}
		q := reader.last
		if !reflect.DeepEqual(q.ModelID, []string{"m1", "m2"}) {
			t.Errorf("%s: model_id = %v, want [m1 m2]", path, q.ModelID)
		}
		if !reflect.DeepEqual(q.Status, []int{429, 503}) {
			t.Errorf("%s: status = %v, want [429 503]", path, q.Status)
		}
		if !reflect.DeepEqual(q.Tags, map[string][]string{"env": {"prod"}, "team": {"a"}}) {
			t.Errorf("%s: tags = %v", path, q.Tags)
		}
	}
}

func TestUsageFacetsTakeRepeatedDims(t *testing.T) {
	h := newFacetsHarness(t, &fakeUsageReader{}, nil)
	for _, query := range []string{"dims=model_id&dims=host_id", "dims=model_id,host_id", "dims=model_id&dims=host_id,team_id"} {
		code, out, body := getFacets(t, h, "root", query)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", query, code, body)
		}
		if _, ok := out.Facets["host_id"]; !ok || len(out.Facets) < 2 {
			t.Errorf("%s: facets = %v, want every listed dim", query, out.Facets)
		}
	}
}
