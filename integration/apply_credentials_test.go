//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/user"
)

// users.update with system.apply is not a grant to sign in as an account: a password or identity set through apply, on another account or on a new one, needs an admin. The refusal comes at plan time, so nothing in the bundle is written.
func TestIntegration_ApplyCredentialsNeedAnAdmin(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	roles := seedBuiltinRoles(t, st)
	editor := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "user-editor", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{
			{Kinds: []string{"users", "teams"}, Verbs: []string{"get", "list", "create", "update"}},
			{Kinds: []string{"system"}, Verbs: []string{"apply"}},
		}}}
	if err := st.stores.Role.Upsert(ctx, editor); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	leadID := st.seedLogin(t, "lead", "pw-lead")
	rootID := st.seedLogin(t, "root", "pw-root")
	st.bindRole(t, "user-editors", editor.Meta.ID, leadID, "system", "")
	st.bindRole(t, "root-admin", roles["admin"].Meta.ID, rootID, "system", "")
	root, err := st.users.Get(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	root.Roles, root.OIDCSubject = []string{user.RoleAdmin}, "https://idp.example.com|root"
	if err := st.users.Upsert(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	before, _ := st.users.Get(ctx, rootID)
	lead := st.login(t, "lead", "pw-lead")

	const hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	team := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Team\nmetadata: {name: core, owner: {kind: system}}\nspec: {}\n---\n"
	userDoc := func(name, spec string) string {
		return team + "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: " + name + "}\nspec: {" + spec + "}\n"
	}
	for name, bundle := range map[string]string{
		"password":                  userDoc("root", "roles: [admin], password: {hash: '"+hash+"'}"),
		"identity":                  userDoc("root", "roles: [admin], identity: {issuer: 'https://idp.example.com', subject: lead}"),
		"new account with password": userDoc("mallory", "password: {hash: '"+hash+"'}"),
		"new account with identity": userDoc("mallory", "identity: {issuer: 'https://idp.example.com', subject: root-2}"),
	} {
		code, raw := lead.doAs(http.MethodPost, "/api/apply", bundle, "application/yaml")
		if code != http.StatusForbidden || strings.Contains(string(raw), `"applied"`) {
			t.Fatalf("%s: apply as a non-admin = %d, want a 403 plan refusal: %s", name, code, raw)
		}
		if teams, err := st.stores.Team.List(ctx); err != nil || len(teams) != 0 {
			t.Fatalf("%s: the refused bundle wrote %d teams (%v)", name, len(teams), err)
		}
		after, _ := st.users.Get(ctx, rootID)
		if after.PasswordHash != before.PasswordHash || after.OIDCSubject != before.OIDCSubject {
			t.Fatalf("%s: the refused bundle changed the account: %+v", name, after)
		}
		if m, _ := st.users.ByUsername(ctx, "mallory"); m != nil {
			t.Fatalf("%s: the refused bundle created %+v", name, m)
		}
	}
	rootSession := st.login(t, "root", "pw-root")
	if code, raw := rootSession.doAs(http.MethodPost, "/api/apply", userDoc("root", "roles: [admin], password: {hash: '"+hash+"'}"), "application/yaml"); code != http.StatusOK {
		t.Fatalf("apply as an admin = %d: %s", code, raw)
	}
	if after, _ := st.users.Get(ctx, rootID); after.PasswordHash != hash {
		t.Fatalf("the admin's apply did not set the password: %+v", after)
	}
}
