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
	"github.com/wyolet/relay/pkg/lifecycle"
)

// refusedRow is what a refused request's row says: the answer, and the model it names.
type refusedRow struct {
	Status          int
	Kind            string
	Model, Provider string
}

// A pin of a snapshot name at a host its model is not served at answers model_not_found, whether written as "@host" or sent as X-WR-Upstream-Host, and never routes to another model that declares the snapshot name as an alias; the row still names the snapshot's model.
func TestDispatch_UnservedPinNeverRoutesThroughAnotherModelsAlias(t *testing.T) {
	upURL, calls := hopUpstream(t, http.StatusOK, `{"answer":true}`)
	var w plantest.World
	acme := w.Provider("acme")
	h := w.Host("host-a", upURL)
	w.Bind(w.Model(acme, "m1", "m1-remote"), h, adapters.OpenAI, "m1-remote")
	m2 := w.Model(w.Provider("other"), "m2")
	m2.Spec.Aliases = []string{"m1"}
	w.Bind(m2, h, adapters.OpenAI)
	w.Credential(h, nil, plantest.System, "sk-a")
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
	rows := make(chan refusedRow, 4)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "row", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		rows <- refusedRow{ev.Status, ev.ErrorKind, lc.ModelName, lc.ProviderName}
		return nil, nil
	}})
	d.Lifecycle = reg

	for name, tc := range map[string]struct {
		model  string
		header string
	}{
		"pinned in the model": {model: "m1@host-a"},
		"pinned by header":    {model: "m1", header: "host-a"},
	} {
		t.Run(name, func(t *testing.T) {
			r := withNormalContext(httptest.NewRequest(http.MethodPost, "/", nil), pr)
			if tc.header != "" {
				r.Header.Set(httpheader.HeaderUpstreamHost, tc.header)
			}
			rec := httptest.NewRecorder()
			Dispatch(d, rec, r, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"` + tc.model + `","stream":false}`), ModelName: tc.model})
			var got refusedRow
			select {
			case got = <-rows:
			case <-time.After(5 * time.Second):
				t.Fatal("no usage row")
			}
			if diff := cmp.Diff(refusedRow{Status: http.StatusNotFound, Kind: "model_not_found", Model: "m1", Provider: "acme"}, got); diff != "" {
				t.Errorf("row (-want +got):\n%s\nresponse %d %s", diff, rec.Code, rec.Body)
			}
			if rec.Code != http.StatusNotFound {
				t.Errorf("status %d, want 404: %s", rec.Code, rec.Body)
			}
			if c := calls(); len(c) > 0 {
				t.Errorf("upstream called %+v", c)
			}
		})
	}
}
