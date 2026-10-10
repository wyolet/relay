//go:build integration

package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/ids"
)

// The whole control API in roles mode over Postgres: a signed-in user with no
// role binding cannot create a host; one bound to catalog-editor can, and can
// edit it.
func TestHostWritesNeedACatalogRole(t *testing.T) {
	ctx := t.Context()
	pool := storagetest.Pool(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	if err := role.SeedBuiltins(ctx, stores.Role, nil, nil); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	roles, err := stores.Role.List(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	var editorRoleID string
	for _, r := range roles {
		if r.Meta.Name == "catalog-editor" {
			editorRoleID = r.Meta.ID
		}
	}
	if editorRoleID == "" {
		t.Fatal("no catalog-editor role seeded")
	}

	users := user.NewStore(gen.New(pool))
	editorID, nobodyID := ids.New(), ids.New()
	for id, name := range map[string]string{editorID: "editor", nobodyID: "nobody"} {
		if err := users.Upsert(ctx, &user.User{ID: id, Username: name}); err != nil {
			t.Fatalf("upsert user %s: %v", name, err)
		}
	}
	global := meta.Owner{Kind: meta.OwnerSystem}
	yes := true
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "editor", Owner: global}}
	rb.Spec.RoleID = editorRoleID
	rb.Spec.Scope = global
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: editorID}}
	rb.Spec.Enabled = &yes
	if err := stores.RoleBinding.Upsert(ctx, rb); err != nil {
		t.Fatalf("upsert role binding: %v", err)
	}
	if err := cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	deps := mountDeps(t)
	deps.Stores = stores
	deps.Catalog = cat
	deps.Users = users
	deps.Authz = audit.Authorizer{Inner: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, Snap: cat.Current}

	actors := map[string]*actor.Actor{
		"nobody": {UserID: nobodyID, Username: "nobody", SessionID: "s-nobody", Subjects: appcatalog.UserSubjects(nobodyID, nil, nil)},
		"editor": {UserID: editorID, Username: "editor", SessionID: "s-editor", Subjects: appcatalog.UserSubjects(editorID, nil, nil)},
	}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-User", actors))
	Mount(r, deps)

	send := func(who, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-User", who)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := send("nobody", http.MethodPost, "/hosts", hostBody("nobodys-box", "")); w.Code != http.StatusForbidden {
		t.Fatalf("no-role create = %d, want 403: %s", w.Code, w.Body)
	}
	if w := send("nobody", http.MethodPost, "/hosts", hostBody("nobodys-box", `,"owner":{"kind":"user"}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("no-role create naming itself owner = %d, want 400: %s", w.Code, w.Body)
	}

	w := send("editor", http.MethodPost, "/hosts", hostBody("shared-box", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("catalog-editor create = %d, want 201: %s", w.Code, w.Body)
	}
	id := createdID(t, w.Body)
	if w := send("editor", http.MethodPut, "/hosts/by-id/"+id, hostBody("shared-box", "")); w.Code != http.StatusOK {
		t.Fatalf("catalog-editor update = %d, want 200: %s", w.Code, w.Body)
	}
	stored, err := stores.Host.Get(ctx, id)
	if err != nil || stored == nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Meta.Owner.Tenant() {
		t.Fatalf("stored host owner = %+v, want no tenant owner", stored.Meta.Owner)
	}
}
