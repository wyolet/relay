//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
)

// A SCIM token creates accounts and decides group membership, so managing SCIM clients takes an admin: a custom role holding every scim-clients verb is refused. A revoked client's name is not issued again, so no new token takes over what the old one provisioned.
func TestIntegration_SCIMClientsAreAdminOnly(t *testing.T) {
	t.Parallel()
	st := newStackAuthz(t, "rbac")
	ctx := context.Background()
	manager := &role.Role{Meta: meta.Metadata{ID: meta.NewID(), Name: "scim-manager", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"scim-clients"}, Verbs: []string{"list", "create", "revoke"}}}}}
	if err := st.stores.Role.Upsert(ctx, manager); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	leadID := st.seedLogin(t, "lead", "pw-lead")
	st.bindRole(t, "scim-managers", manager.Meta.ID, leadID, "system", "")
	if err := st.cat.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`); code != http.StatusCreated {
		t.Fatalf("admin create = %d: %s", code, raw)
	}

	lead := st.login(t, "lead", "pw-lead")
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/scim-clients", ""},
		{http.MethodPost, "/api/scim-clients", `{"name":"entra"}`},
		{http.MethodPost, "/api/scim-clients/okta/revoke", ""},
	} {
		if code, raw := lead.do(req.method, req.path, req.body); code != http.StatusForbidden {
			t.Fatalf("%s %s as a non-admin = %d, want 403: %s", req.method, req.path, code, raw)
		}
	}
	if code, raw := st.adminDo(http.MethodGet, "/api/scim-clients", ""); code != http.StatusOK || string(raw) == "" {
		t.Fatalf("admin list = %d: %s", code, raw)
	}

	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients/okta/revoke", ""); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", code, raw)
	}
	if code, raw := st.adminDo(http.MethodPost, "/api/scim-clients", `{"name":"okta"}`); code != http.StatusConflict {
		t.Fatalf("reissuing a revoked name = %d, want 409: %s", code, raw)
	}
}
