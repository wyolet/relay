package control

import (
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/meta"
)

// An organization draft is previewed like a system one, by an admin alone.
func TestReachable_OrganizationDraft(t *testing.T) {
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha := w.Host("host-a", "http://a.example")
	w.Bind(w.Model(prov, "m1"), ha, adapters.OpenAI)
	pa := w.Pool(ha, w.Credential(ha, w.Account(ha), plantest.System, "sk-a"))
	r, api := testAPI(t, map[string]*actor.Actor{"u": {UserID: meta.NewID(), Username: "u"}, "root": scopeActors["root"]})
	registerReachable(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}, PlanView: w.Load(t).Engine.View}, nil)
	path := "/catalog/reachable?owner=organization&pool=" + pa.Meta.ID
	var got Reachable
	wantJSON(t, scopeReq(t, r, "root", http.MethodGet, path, ""), http.StatusOK, &got)
	if len(got.Models) != 1 {
		t.Fatalf("admin read %+v, want the one model pool a brings", got)
	}
	wantAPIError(t, scopeReq(t, r, "u", http.MethodGet, path, ""), http.StatusForbidden, "", "you may not write a policy owned by organization")
}
