//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

// The admin token names no user, so what it creates — through CRUD or
// apply — is the organization's, not a personal row belonging to nobody.
func TestIntegration_AdminTokenCreatesOrganizationRows(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()

	code, raw := st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"ci-crud"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Metadata.Owner.Kind != string(meta.OwnerOrganization) || row.Metadata.Owner.ID != "" {
		t.Fatalf("CRUD owner = %+v, want the organization", row.Metadata.Owner)
	}

	const doc = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Policy
metadata:
  name: ci-apply
  owner: {kind: user}
spec: {}
`
	if code, _, raw := st.applyBundle(doc, ""); code != http.StatusOK {
		t.Fatalf("apply = %d: %s", code, raw)
	}
	pols, err := st.stores.Policy.List(ctx)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	for _, p := range pols {
		if p.Meta.Name == "ci-apply" {
			if p.Meta.Owner != meta.Global {
				t.Fatalf("apply owner = %+v, want the organization", p.Meta.Owner)
			}
			return
		}
	}
	t.Fatal("applied policy not found")
}

// An admin may edit an organization row through CRUD, and the edit is marked
// dirty. A non-admin can neither edit nor delete one; an admin may delete one
// nothing references.
func TestIntegration_OrganizationRowEditsAreAdminOnly(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()

	code, raw := st.adminDo(http.MethodPost, "/api/policies", `{"metadata":{"name":"shared"},"spec":{}}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/policies = %d: %s", code, raw)
	}
	var row tenancyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	path := "/api/policies/by-id/" + row.Metadata.ID
	edit := `{"metadata":{"name":"shared","displayName":"Shared"},"spec":{}}`

	st.seedLogin(t, "dev", "pw-dev")
	dev := st.login(t, "dev", "pw-dev")
	if code, raw := dev.do(http.MethodPut, path, edit); code != http.StatusForbidden {
		t.Fatalf("non-admin PUT = %d, want 403: %s", code, raw)
	}
	if code, raw := dev.do(http.MethodDelete, path, ""); code != http.StatusForbidden {
		t.Fatalf("non-admin DELETE = %d, want 403: %s", code, raw)
	}

	if code, raw := st.adminDo(http.MethodPut, path, edit); code != http.StatusOK {
		t.Fatalf("admin PUT = %d, want 200: %s", code, raw)
	}
	got, err := st.stores.Policy.Get(ctx, row.Metadata.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if !got.Meta.Dirty || got.Meta.DisplayName != "Shared" {
		t.Fatalf("after admin edit: dirty=%v displayName=%q, want dirty and Shared", got.Meta.Dirty, got.Meta.DisplayName)
	}
	if got.Meta.Owner != meta.Global {
		t.Fatalf("owner after edit = %+v, want the organization", got.Meta.Owner)
	}

	if code, raw := st.adminDo(http.MethodDelete, path, ""); code != http.StatusNoContent {
		t.Fatalf("admin DELETE = %d, want 204: %s", code, raw)
	}
}
