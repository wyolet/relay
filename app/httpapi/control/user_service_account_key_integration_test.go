//go:build integration

package control

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// A key of a user's service account is minted and rotated by that user or an admin. A caller who reaches the service account through a role at the global scope, a developer there, is refused both, as for a key of the user's own.
func TestUserServiceAccountKeyIsTheUsersToMint(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	users := user.NewStore(pool)
	bob, hal := &user.User{ID: meta.NewID(), Username: "bob"}, &user.User{ID: meta.NewID(), Username: "hal"}
	for _, u := range []*user.User{bob, hal} {
		must(t, "upsert user", users.Upsert(ctx, u))
	}
	bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "bob-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: bob.ID}}}
	must(t, "upsert service account", stores.ServiceAccount.Upsert(ctx, bot))

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "hal-global-developer"}}
	for _, r := range builtins {
		r.Meta.ID, r.Spec.Enabled = meta.NewID(), &yes
		if r.Meta.Name == "developer" {
			b.Spec.RoleID = r.Meta.ID
		}
	}
	b.Spec.Scope, b.Spec.Enabled = meta.Global, &yes
	b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: hal.ID}}
	b.StampOwner()
	cat := catalogtest.Catalog{Roles: builtins, RoleBindings: []*rolebinding.RoleBinding{b}}.Load(t)
	d := Deps{Authz: authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, Stores: stores, Users: users}
	actors := map[string]*actor.Actor{
		"bob":   {UserID: bob.ID, Username: "bob", Subjects: appcatalog.UserSubjects(bob.ID, nil, nil)},
		"hal":   {UserID: hal.ID, Username: "hal", Subjects: appcatalog.UserSubjects(hal.ID, nil, nil)},
		"token": {AdminToken: true},
	}
	h, api := testAPI(t, actors)
	registerKeyCreate(api, d, nil)
	registerKeyRotate(api, d, nil)
	create := `{"metadata":{"name":"bot-key-%s"},"spec":{"principal":{"kind":"serviceaccount","id":"` + bot.Meta.ID + `"}}}`
	const refused = "authz: forbidden: a key of a user's service account is created and rotated by that user or an admin"

	wantAPIError(t, scopeReq(t, h, "hal", http.MethodPost, "/keys", strings.ReplaceAll(create, "%s", "hal")), http.StatusForbidden, "", refused)
	var made *key.Key
	for _, who := range []string{"bob", "token"} {
		w := scopeReq(t, h, who, http.MethodPost, "/keys", strings.ReplaceAll(create, "%s", who))
		if w.Code != http.StatusCreated {
			t.Fatalf("%s creates a key of bob's service account = %d %s, want 201", who, w.Code, w.Body)
		}
		if made == nil {
			ks, err := stores.Key.List(ctx)
			must(t, "list keys", err)
			made = ks[0]
		}
	}

	rotate := "/keys/by-id/" + made.Meta.ID + "/rotate"
	wantAPIError(t, scopeReq(t, h, "hal", http.MethodPost, rotate, `{}`), http.StatusForbidden, "", refused)
	for _, who := range []string{"bob", "token"} {
		if w := scopeReq(t, h, who, http.MethodPost, rotate, `{}`); w.Code != http.StatusOK {
			t.Errorf("%s rotates a key of bob's service account = %d %s, want 200", who, w.Code, w.Body)
		}
	}
}
