package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/ids"
)

// catalogEditorRBAC is the enforcing authorizer over a catalog holding the
// built-in roles and one binding: catalog-editor at the global scope for
// editorID.
func catalogEditorRBAC(t *testing.T, editorID string) authz.RBAC {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	yes := true
	var editorRoleID string
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		if r.Meta.Name == "catalog-editor" {
			editorRoleID = r.Meta.ID
		}
	}
	global := meta.Owner{Kind: meta.OwnerSystem}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "editor", Owner: global}}
	rb.Spec.RoleID = editorRoleID
	rb.Spec.Scope = global
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: editorID}}
	rb.Spec.Enabled = &yes

	cat := appcatalog.New(
		tokenList[provider.Provider]{}, tokenList[host.Host]{}, tokenList[policy.Policy]{},
		tokenList[model.Model]{}, tokenList[hostkey.HostKey]{}, tokenList[ratelimit.RateLimit]{},
		tokenList[key.Key]{}, tokenList[pricing.Pricing]{}, tokenList[binding.Binding]{},
	)
	cat.UseTenancy(
		tokenList[team.Team]{}, tokenList[project.Project]{},
		tokenList[serviceaccount.ServiceAccount]{}, tokenList[group.Group]{},
		tokenList[role.Role](builtins), tokenList[rolebinding.RoleBinding]{rb},
		tokenList[policybinding.PolicyBinding]{},
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
}

// newSharedCatalogHarness mounts hosts and host bindings, plus the kinds that
// keep personal rows, with the arguments registerCRUD gives them, behind a
// middleware that injects the actor named by X-Test-Actor.
func newSharedCatalogHarness(t *testing.T) http.Handler {
	t.Helper()
	editorID, nobodyID := ids.New(), ids.New()
	actors := map[string]*actor.Actor{
		"nobody": {UserID: nobodyID, Username: "nobody", Subjects: appcatalog.UserSubjects(nobodyID, nil, nil)},
		"editor": {UserID: editorID, Username: "editor", Subjects: appcatalog.UserSubjects(editorID, nil, nil)},
		"token":  {AdminToken: true, Username: "admin-token"},
	}
	rbac := catalogEditorRBAC(t, editorID)
	d := Deps{Authz: rbac}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := actors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	cfg := huma.DefaultConfig("shared-catalog-test", "0")
	cfg.OpenAPI.Components.Schemas = httpapi.NewRegistry()
	api := humachi.New(r, cfg)

	hmeta := func(h *host.Host) *meta.Metadata { return &h.Meta }
	hosts := &memStore[host.Host]{metaOf: hmeta, items: map[string]*host.Host{}}
	registerKind[host.Host](api, "hosts", "host", hosts, rbac, hmeta,
		func(h *host.Host) error { return h.Validate() }, "",
		listScanResolver[host.Host](hosts, hmeta), nil, nil, nil, nil, nil, noSettings{}, false, nil, nil)

	bmeta := func(b *binding.Binding) *meta.Metadata { return &b.Meta }
	bindings := &memStore[binding.Binding]{metaOf: bmeta, items: map[string]*binding.Binding{}}
	registerKind[binding.Binding](api, "host-bindings", "host-binding", bindings, rbac, bmeta,
		func(b *binding.Binding) error { return b.Validate() }, "",
		listScanResolver[binding.Binding](bindings, bmeta), nil, nil, nil, nil, nil, noSettings{}, false, nil, nil)

	kmeta := func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta }
	hostKeys := &memStore[hostkey.HostKey]{metaOf: kmeta, items: map[string]*hostkey.HostKey{}}
	registerKind[hostkey.HostKey](api, "host-keys", "host-key", hostKeys, rbac, kmeta,
		func(k *hostkey.HostKey) error { return k.Validate() }, meta.OwnerUser,
		listScanResolver[hostkey.HostKey](hostKeys, kmeta), guardHostKey(d), nil, nil, nil, nil, noSettings{}, false, nil, nil)

	rlmeta := func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }
	limits := &memStore[ratelimit.RateLimit]{metaOf: rlmeta, items: map[string]*ratelimit.RateLimit{}}
	registerKind[ratelimit.RateLimit](api, "rate-limits", "rate-limit", limits, rbac, rlmeta,
		func(r *ratelimit.RateLimit) error { return r.Validate() }, meta.OwnerUser,
		listScanResolver[ratelimit.RateLimit](limits, rlmeta), nil, nil, nil, nil, nil, noSettings{}, false, nil, nil)

	polmeta := func(p *policy.Policy) *meta.Metadata { return &p.Meta }
	policies := &memStore[policy.Policy]{metaOf: polmeta, items: map[string]*policy.Policy{}}
	registerKind[policy.Policy](api, "policies", "policy", policies, rbac, polmeta,
		func(p *policy.Policy) error { return p.Validate() }, meta.OwnerUser,
		listScanResolver[policy.Policy](policies, polmeta), guardPolicyModels(d), nil, nil, nil, nil, noSettings{}, false, nil, nil)

	return r
}

const sharedCatalogMessage = "is shared catalog data and cannot be owned by a user"

func hostBody(name, owner string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q%s},"spec":{"baseURL":"https://upstream.example"}}`, name, owner)
}

func bindingBody(name, owner string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q%s},"spec":{"modelId":%q,"hostId":%q,"adapter":"openai"}}`,
		name, owner, ids.New(), ids.New())
}

func createdID(t *testing.T, w interface{ Bytes() []byte }) string {
	t.Helper()
	var out struct {
		Metadata meta.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(w.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Metadata.ID
}

// A body naming a user owner for a host or binding is a bad request for any
// caller, on create and on update.
func TestSharedCatalogKindsRefuseUserOwnerInBody(t *testing.T) {
	h := newSharedCatalogHarness(t)
	const userOwner = `,"owner":{"kind":"user"}`
	for _, tc := range []struct {
		plural string
		body   func(name, owner string) string
	}{
		{"hosts", hostBody},
		{"host-bindings", bindingBody},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			for _, who := range []string{"nobody", "editor", "token"} {
				w := scopeReq(t, h, who, http.MethodPost, "/"+tc.plural, tc.body("mine", userOwner))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), sharedCatalogMessage) {
					t.Errorf("%s POST with a user owner = %d %s, want 400 with the shared catalog message", who, w.Code, w.Body)
				}
			}

			w := scopeReq(t, h, "editor", http.MethodPost, "/"+tc.plural, tc.body("shared", ""))
			if w.Code != http.StatusCreated {
				t.Fatalf("editor create = %d: %s", w.Code, w.Body)
			}
			id := createdID(t, w.Body)
			for _, who := range []string{"editor", "token"} {
				w := scopeReq(t, h, who, http.MethodPut, "/"+tc.plural+"/by-id/"+id, tc.body("shared", userOwner))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), sharedCatalogMessage) {
					t.Errorf("%s PUT with a user owner = %d %s, want 400 with the shared catalog message", who, w.Code, w.Body)
				}
			}
		})
	}
}

// Writing a host or binding takes a role: a signed-in user with no binding is
// refused, a catalog-editor and the admin token are let through.
func TestSharedCatalogKindsWriteAccess(t *testing.T) {
	h := newSharedCatalogHarness(t)
	for _, tc := range []struct {
		plural string
		body   func(name, owner string) string
	}{
		{"hosts", hostBody},
		{"host-bindings", bindingBody},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			if w := scopeReq(t, h, "nobody", http.MethodPost, "/"+tc.plural, tc.body("nobodys", "")); w.Code != http.StatusForbidden {
				t.Errorf("no-role create = %d, want 403: %s", w.Code, w.Body)
			}
			for _, who := range []string{"editor", "token"} {
				w := scopeReq(t, h, who, http.MethodPost, "/"+tc.plural, tc.body(who+"-row", ""))
				if w.Code != http.StatusCreated {
					t.Fatalf("%s create = %d, want 201: %s", who, w.Code, w.Body)
				}
				id := createdID(t, w.Body)
				if w := scopeReq(t, h, who, http.MethodPut, "/"+tc.plural+"/by-id/"+id, tc.body(who+"-row-renamed", "")); w.Code != http.StatusOK {
					t.Errorf("%s update = %d, want 200: %s", who, w.Code, w.Body)
				}
				// A row created with no owner is not readable without a role
				// either, so the refusal may answer as absent.
				if w := scopeReq(t, h, "nobody", http.MethodPut, "/"+tc.plural+"/by-id/"+id, tc.body("taken", "")); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
					t.Errorf("no-role update of %s's row = %d, want 403 or 404: %s", who, w.Code, w.Body)
				}
			}
		})
	}
}

// Host keys, rate limits and policies keep personal rows: a user with no
// role creates them as their own.
func TestPersonalKindsStillCreatedAsCallersOwn(t *testing.T) {
	h := newSharedCatalogHarness(t)
	for _, tc := range []struct {
		plural, body string
	}{
		{"host-keys", fmt.Sprintf(`{"metadata":{"name":"my-key"},"spec":{"hostId":%q,"policyId":%q,"valueFrom":{"kind":"stored"},"value":"sk-test"}}`, ids.New(), ids.New())},
		{"rate-limits", `{"metadata":{"name":"my-limit"},"spec":{"rules":[{"meter":"requests","amount":10,"window":60,"strategy":"token-bucket"}]}}`},
		{"policies", `{"metadata":{"name":"my-policy"},"spec":{}}`},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			w := scopeReq(t, h, "nobody", http.MethodPost, "/"+tc.plural, tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
			}
			var out struct {
				Metadata meta.Metadata `json:"metadata"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Metadata.Owner.Kind != meta.OwnerUser || out.Metadata.Owner.ID == "" {
				t.Fatalf("owner = %+v, want the caller's user", out.Metadata.Owner)
			}
		})
	}
}
