package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/httpheader"
)

// Unchanged behaviour: a snapshot name pinned at a host where its model has no binding at all is another model's alias pin there, and routes to that model, whether written as "@host" or sent as X-WR-Upstream-Host.
func TestDispatch_SnapshotNamePinnedWhereItsModelIsUnboundRoutesThroughAlias(t *testing.T) {
	h1URL, h1Calls := hopUpstream(t, http.StatusOK, `{"answer":true}`)
	h2URL, h2Calls := hopUpstream(t, http.StatusOK, `{"answer":true}`)
	var w plantest.World
	acme := w.Provider("acme")
	h1, h2 := w.Host("h1", h1URL), w.Host("h2", h2URL)
	w.Bind(w.Model(acme, "foo"), h1, adapters.OpenAI)
	b := w.Model(acme, "b")
	b.Spec.Aliases = []string{"foo"}
	w.Bind(b, h2, adapters.OpenAI)
	w.Credential(h1, nil, plantest.System, "sk-1")
	w.Credential(h2, nil, plantest.System, "sk-2")
	k := w.UserKey(w.User(), "rk-secret")
	plans := w.Load(t)
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatal("fixture key has no plan")
	}
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, HostKeys: w.Credentials, Keys: []*key.Key{k}, Bindings: w.Bindings}.Load(t)
	d := buildRunnableDeps(t, cat)
	d.Plans = plans.Holder
	pr := principalOf(kp, k.Spec.KeyHash)

	for name, tc := range map[string]struct{ model, header string }{
		"pinned in the model": {model: "foo@h2"},
		"pinned by header":    {model: "foo", header: "h2"},
	} {
		t.Run(name, func(t *testing.T) {
			r := withNormalContext(httptest.NewRequest(http.MethodPost, "/", nil), pr)
			if tc.header != "" {
				r.Header.Set(httpheader.HeaderUpstreamHost, tc.header)
			}
			rec := httptest.NewRecorder()
			Dispatch(d, rec, r, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"` + tc.model + `","stream":false}`), ModelName: tc.model})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if diff := cmp.Diff([]hopCall{{"/v1/chat/completions", "foo"}}, h2Calls()); diff != "" {
				t.Errorf("h2 calls (-want +got):\n%s", diff)
			}
			if c := h1Calls(); len(c) > 0 {
				t.Errorf("h1 called %+v", c)
			}
		})
	}
}
