//go:build integration

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/internal/catalog"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// testMasterKey is a 32-byte hex key used in integration tests.
const testMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

func buildSecretTestServer(t *testing.T, withMasterKey bool) (*httptest.Server, *catalog.PGStore, *storagemod.Storage) {
	t.Helper()
	ctx := context.Background()
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)

	// Seed a default provider so catalog validator passes.
	st := storagemod.MustOpenStorage(ctx, t, dsn)
	if err := storagemod.SeedProviderRow(ctx, st,
		"seed-prov", `{"Name":"seed-prov"}`, `{"kind":"ollama","baseURL":"http://localhost:11434","default":true}`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	var mk []byte
	if withMasterKey {
		mk = make([]byte, 32)
		for i := range mk {
			mk[i] = byte(i + 1)
		}
	}
	store, err := catalog.NewPGStoreNoReload(st.Catalog, st)
	if err != nil {
		t.Fatalf("configstore: %v", err)
	}
	store.SetMasterKey(mk)
	if err := store.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.DefaultMaxRequestBytes))

	deps := crudDeps(st, store)
	kinds := buildAdminKinds(store, st)
	crudH := buildAdminCRUD(kinds, deps, store, nil)

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	authMW := auth.Middleware(nil, nil, nil)
	mountHuma(r, authMW, nil, stub, stub, stub, stub)
	_ = crudH
	_ = adminTestToken

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store, st
}

// --- Secret tests ---

func TestAdminSecret_EnvMode_CRUD(t *testing.T) {
	t.Setenv("RELAY_TEST_KEY", "test-value-1234")

	srv, store, _ := buildSecretTestServer(t, false)

	body := map[string]any{
		"name":     "test-env-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind": "env",
			"env":  "RELAY_TEST_KEY",
		},
	}

	// Create → 201
	resp := adminReq(t, srv, http.MethodPost, "/control/secrets", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create: want 201 got %d", resp.StatusCode)
	}
	var created map[string]any
	decodeResp(t, resp, &created)

	// Response must have kind=env and env=RELAY_TEST_KEY; no cleartext field
	vf, _ := created["valueFrom"].(map[string]any)
	if vf["kind"] != "env" {
		t.Errorf("want kind=env, got %v", vf["kind"])
	}
	if vf["env"] != "RELAY_TEST_KEY" {
		t.Errorf("want env=RELAY_TEST_KEY, got %v", vf["env"])
	}
	if _, hasVal := vf["value"]; hasVal {
		t.Error("response must not contain a cleartext value field")
	}
	if _, hasMasked := vf["value_masked"]; hasMasked {
		t.Error("env-mode response must not contain value_masked")
	}

	// GET → 200, same shape
	resp = adminReq(t, srv, http.MethodGet, "/control/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: want 200 got %d", resp.StatusCode)
	}
	var got map[string]any
	decodeResp(t, resp, &got)
	vf2, _ := got["valueFrom"].(map[string]any)
	if vf2["kind"] != "env" {
		t.Errorf("get: want kind=env, got %v", vf2["kind"])
	}
	if _, hasVal := vf2["value"]; hasVal {
		t.Error("get response must not contain cleartext value")
	}

	// Auto-reload: secret in snapshot
	sec, ok := store.SecretByName("test-env-secret")
	if !ok {
		t.Fatal("auto-reload: secret not in snapshot")
	}
	if sec.Resolved != "test-value-1234" {
		t.Errorf("resolved: got %q", sec.Resolved)
	}

	// Update → 200, different env var
	t.Setenv("RELAY_TEST_KEY2", "another-value-5678")
	updateBody := map[string]any{
		"name":     "test-env-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind": "env",
			"env":  "RELAY_TEST_KEY2",
		},
	}
	resp = adminReq(t, srv, http.MethodPut, "/control/secrets/test-env-secret", updateBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update: want 200 got %d", resp.StatusCode)
	}
	var updated map[string]any
	decodeResp(t, resp, &updated)
	vf3, _ := updated["valueFrom"].(map[string]any)
	if vf3["env"] != "RELAY_TEST_KEY2" {
		t.Errorf("update: want env=RELAY_TEST_KEY2, got %v", vf3["env"])
	}

	// GET reflects change
	resp = adminReq(t, srv, http.MethodGet, "/control/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get after update: want 200 got %d", resp.StatusCode)
	}
	var got2 map[string]any
	decodeResp(t, resp, &got2)
	vf4, _ := got2["valueFrom"].(map[string]any)
	if vf4["env"] != "RELAY_TEST_KEY2" {
		t.Errorf("get after update: want RELAY_TEST_KEY2, got %v", vf4["env"])
	}
	resp.Body.Close()

	// Delete → 204
	resp = adminReq(t, srv, http.MethodDelete, "/control/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET after delete → 404
	resp = adminReq(t, srv, http.MethodGet, "/control/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminSecret_StoredMode_WithMasterKey(t *testing.T) {
	ctx := context.Background()
	srv, store, st := buildSecretTestServer(t, true)
	_ = store // used below for SecretByName/PolicyByName

	body := map[string]any{
		"name":     "test-stored-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-testapikey1234",
		},
	}

	// Create → 201
	resp := adminReq(t, srv, http.MethodPost, "/control/secrets", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create stored: want 201 got %d", resp.StatusCode)
	}
	var created map[string]any
	decodeResp(t, resp, &created)

	vf, _ := created["valueFrom"].(map[string]any)
	if vf["kind"] != "stored" {
		t.Errorf("want kind=stored, got %v", vf["kind"])
	}
	masked, _ := vf["value_masked"].(string)
	if masked == "" {
		t.Error("want value_masked in response")
	}
	if _, hasVal := vf["value"]; hasVal {
		t.Error("response must not contain cleartext value")
	}
	if len(masked) < 8 {
		t.Errorf("masked too short: %q", masked)
	}

	// Verify PG row has ciphertext populated.
	ct, envNull, err := storagemod.QuerySecretStoredRow(ctx, st, "test-stored-secret")
	if err != nil {
		t.Fatalf("scan pg row: %v", err)
	}
	if len(ct) == 0 {
		t.Error("want non-empty ciphertext in PG")
	}
	if envNull != nil {
		t.Error("want NULL value_from_env for stored-mode")
	}

	// GET → 200, value_masked present, no cleartext
	resp = adminReq(t, srv, http.MethodGet, "/control/secrets/test-stored-secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get stored: want 200 got %d", resp.StatusCode)
	}
	var got map[string]any
	decodeResp(t, resp, &got)
	vf2, _ := got["valueFrom"].(map[string]any)
	if _, hasVal := vf2["value"]; hasVal {
		t.Error("get: must not expose cleartext")
	}
	if vf2["value_masked"] == "" {
		t.Error("get: want value_masked")
	}

	// Update stored-mode with new value → ciphertext changes
	updateBody := map[string]any{
		"name":     "test-stored-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-newvalue9876",
		},
	}
	resp = adminReq(t, srv, http.MethodPut, "/control/secrets/test-stored-secret", updateBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update stored: want 200 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	ct2, err := storagemod.QuerySecretStoredCiphertext(ctx, st, "test-stored-secret")
	if err != nil {
		t.Fatalf("scan pg row after update: %v", err)
	}
	if string(ct2) == string(ct) {
		t.Error("ciphertext should change after update")
	}
}

func TestAdminSecret_StoredMode_NoMasterKey_400(t *testing.T) {
	srv, _, _ := buildSecretTestServer(t, false) // no master key

	body := map[string]any{
		"name":     "bad-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-somevalue",
		},
	}

	resp := adminReq(t, srv, http.MethodPost, "/control/secrets", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", resp.StatusCode)
	}
	// huma path returns 400 with message (no code field in huma error shape)
	var errOut map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&errOut); err == nil {
		errInner, _ := errOut["error"].(map[string]any)
		if errInner["message"] == "" {
			t.Error("want error message in response")
		}
	}
}

func TestAdminSecret_DeleteReferenced_400(t *testing.T) {
	t.Setenv("RELAY_SEC_REF", "policy-ref-value")
	srv, _, _ := buildSecretTestServer(t, false)

	secBody := map[string]any{
		"name": "ref-secret", "provider": "seed-prov",
		"valueFrom": map[string]any{"kind": "env", "env": "RELAY_SEC_REF"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/secrets", secBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create secret: want 201 got %d", resp.StatusCode)
	}

	poolBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Policy",
		"metadata": map[string]string{"name": "ref-policy"},
		"spec":     map[string]any{"provider": "seed-prov", "secrets": []string{"ref-secret"}},
	}
	resp = adminReq(t, srv, http.MethodPost, "/control/policies", poolBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create policy: want 201 got %d", resp.StatusCode)
	}

	resp = adminReq(t, srv, http.MethodDelete, "/control/secrets/ref-secret", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete referenced secret: want 400 got %d", resp.StatusCode)
	}

	// Secret must still be present (no PG mutation happened).
	resp2 := adminReq(t, srv, http.MethodGet, "/control/secrets/ref-secret", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("secret should still exist after rejected delete: got %d", resp2.StatusCode)
	}
}

func TestAdminSecret_List(t *testing.T) {
	t.Setenv("RELAY_LIST_KEY", "list-value")

	srv, _, _ := buildSecretTestServer(t, false)

	// Initially empty
	resp := adminReq(t, srv, http.MethodGet, "/control/secrets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list empty: want 200 got %d", resp.StatusCode)
	}
	var listOut struct{ Items []map[string]any }
	decodeResp(t, resp, &listOut)
	if len(listOut.Items) != 0 {
		t.Errorf("want 0 items, got %d", len(listOut.Items))
	}

	// Create one
	secBody := map[string]any{
		"name": "list-secret", "provider": "seed-prov",
		"valueFrom": map[string]any{"kind": "env", "env": "RELAY_LIST_KEY"},
	}
	resp = adminReq(t, srv, http.MethodPost, "/control/secrets", secBody)
	resp.Body.Close()

	// List again
	resp = adminReq(t, srv, http.MethodGet, "/control/secrets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200 got %d", resp.StatusCode)
	}
	var listOut2 struct{ Items []map[string]any }
	decodeResp(t, resp, &listOut2)
	if len(listOut2.Items) != 1 {
		t.Errorf("want 1 item, got %d", len(listOut2.Items))
	}
	item := listOut2.Items[0]
	if vf, _ := item["valueFrom"].(map[string]any); vf["kind"] != "env" {
		t.Errorf("list item: want kind=env")
	}
}

// --- Attachment (derived view) tests ---

// TestAdminAttachment_DerivedView verifies GET /admin/attachments derives from inline spec.
// Attachments are created by PUTting a Policy with rateLimits inline.
func TestAdminAttachment_DerivedView(t *testing.T) {
	srv, store, _ := buildSecretTestServer(t, false)

	// Create a ratelimit.
	rlBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "RateLimit",
		"metadata": map[string]string{"name": "att-rl"},
		"spec": map[string]any{
			"strategy": "sliding-window",
			"window":   60000000000,
			"amount":   100,
		},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/ratelimits", rlBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ratelimit: want 201 got %d", resp.StatusCode)
	}

	// Create a policy WITH inline rateLimits.
	poolBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Policy",
		"metadata": map[string]string{"name": "att-policy"},
		"spec": map[string]any{
			"provider":          "seed-prov",
			"skipDefaultLimits": true,
			"rateLimits":        []map[string]any{{"ref": "att-rl", "meter": "requests"}},
		},
	}
	resp = adminReq(t, srv, http.MethodPost, "/control/policies", poolBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create policy with rateLimits: want 201 got %d", resp.StatusCode)
	}

	// Verify snapshot reflects inline rateLimits.
	policy, ok := store.PolicyByName("att-policy")
	if !ok {
		t.Fatal("policy not in snapshot")
	}
	if len(policy.Spec.RateLimits) == 0 {
		t.Error("policy spec missing RateLimits")
	} else if policy.Spec.RateLimits[0].Ref != "att-rl" {
		t.Errorf("want ref=att-rl, got %q", policy.Spec.RateLimits[0].Ref)
	}

	// GET /admin/attachments — all → includes our policy attachment.
	resp = adminReq(t, srv, http.MethodGet, "/control/attachments", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list all: want 200 got %d", resp.StatusCode)
	}
	var listAll struct{ Items []map[string]any }
	decodeResp(t, resp, &listAll)
	found := false
	for _, item := range listAll.Items {
		if item["parentKind"] == "Policy" && item["parentName"] == "att-policy" && item["ratelimitName"] == "att-rl" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected attachment for att-policy in list-all response, got %+v", listAll.Items)
	}

	// GET /admin/attachments?parent_kind=Policy&parent_name=att-policy → filtered.
	resp = adminReq(t, srv, http.MethodGet, "/control/attachments?parent_kind=Policy&parent_name=att-policy", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list filtered: want 200 got %d", resp.StatusCode)
	}
	var listFiltered struct{ Items []map[string]any }
	decodeResp(t, resp, &listFiltered)
	if len(listFiltered.Items) != 1 {
		t.Errorf("want 1 item, got %d", len(listFiltered.Items))
	}
	item := listFiltered.Items[0]
	if item["ratelimitName"] != "att-rl" {
		t.Errorf("want ratelimitName=att-rl, got %v", item["ratelimitName"])
	}
	if item["meter"] != "requests" {
		t.Errorf("want meter=requests, got %v", item["meter"])
	}
	expectedID := "Policy:att-policy:att-rl:requests"
	if item["id"] != expectedID {
		t.Errorf("want id=%q, got %q", expectedID, item["id"])
	}

	// Removing the rateLimits via PUT removes it from the derived view.
	poolBodyNoRL := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Policy",
		"metadata": map[string]string{"name": "att-policy"},
		"spec": map[string]any{
			"provider":          "seed-prov",
			"skipDefaultLimits": true,
		},
	}
	resp = adminReq(t, srv, http.MethodPut, "/control/policies/att-policy", poolBodyNoRL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update policy: want 200 got %d", resp.StatusCode)
	}

	resp = adminReq(t, srv, http.MethodGet, "/control/attachments?parent_kind=Policy&parent_name=att-policy", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list after remove: want 200 got %d", resp.StatusCode)
	}
	var listEmpty struct{ Items []map[string]any }
	decodeResp(t, resp, &listEmpty)
	if len(listEmpty.Items) != 0 {
		t.Errorf("want 0 items after removing rateLimits, got %d", len(listEmpty.Items))
	}
}

// TestAdminAttachment_NoWriteEndpoints verifies write methods (POST, remove) on /admin/attachments return 404/405.
func TestAdminAttachment_NoWriteEndpoints(t *testing.T) {
	srv, _, _ := buildSecretTestServer(t, false)

	// POST /admin/attachments should now 404 (route is gone).
	attBody := map[string]any{"parentKind": "Policy", "parentName": "x", "ratelimitName": "y", "meter": "requests"}
	resp := adminReq(t, srv, http.MethodPost, "/control/attachments", attBody)
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		t.Error("POST /admin/attachments should not succeed (write API removed)")
	}
}

// TestAdminAttachment_MissingQueryParams_400 verifies the filter validation.
func TestAdminAttachment_MissingQueryParams_400(t *testing.T) {
	srv, _, _ := buildSecretTestServer(t, false)

	resp := adminReq(t, srv, http.MethodGet, "/control/attachments?parent_kind=Policy", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing parent_name: want 400 got %d", resp.StatusCode)
	}
}

// TestAdminSecret_OpenAPISchema_NoCleartextField verifies the response shape.
func TestAdminSecret_OpenAPISchema_NoCleartextField(t *testing.T) {
	srv, _, _ := buildSecretTestServer(t, false)

	resp := adminReq(t, srv, http.MethodGet, "/control/secrets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200 got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeResp(t, resp, &body)
	items, _ := body["items"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		vf, _ := m["valueFrom"].(map[string]any)
		if _, has := vf["value"]; has {
			t.Error("OpenAPI/response: cleartext 'value' field must not appear in response")
		}
	}
}

// TestAdminSecret_InvalidProvider_Rejected verifies that POSTing a secret with
// an unknown provider is rejected with 400 before any row is written to PG.
// This covers the bug where ValidateWithPatch ran only on Reload (post-commit),
// allowing an invalid row to poison subsequent catalog reloads cluster-wide.
func TestAdminSecret_InvalidProvider_Rejected(t *testing.T) {
	t.Setenv("RELAY_TEST_INVALID", "somevalue")

	srv, _, st := buildSecretTestServer(t, false)
	ctx := context.Background()

	cases := []struct {
		name     string
		provider string
	}{
		{"empty provider defaults to 'default' which is unknown", ""},
		{"explicitly nonexistent provider", "nonexistent-provider-xyz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"name":     "bad-secret",
				"provider": tc.provider,
				"valueFrom": map[string]any{
					"kind": "env",
					"env":  "RELAY_TEST_INVALID",
				},
			}

			resp := adminReq(t, srv, http.MethodPost, "/control/secrets", body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("want 400, got %d", resp.StatusCode)
			}

			// No row must have been written to PG.
			count, err := storagemod.CountSecrets(ctx, st, "bad-secret")
			if err != nil {
				t.Fatalf("CountSecrets: %v", err)
			}
			if count != 0 {
				t.Errorf("want 0 PG rows for rejected secret, got %d", count)
			}
		})
	}
}
