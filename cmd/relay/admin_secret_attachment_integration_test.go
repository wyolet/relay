//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// testMasterKey is a 32-byte hex key used in integration tests.
const testMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

func buildSecretTestServer(t *testing.T, withMasterKey bool) (*testServer, *configstore.PGStore) {
	t.Helper()
	ctx := context.Background()
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)

	pool, err := configstore.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	// Seed a default provider so catalog validator passes.
	_, err = pool.Exec(ctx, `
		INSERT INTO providers (name, metadata, spec)
		VALUES ('seed-prov', '{"Name":"seed-prov"}', '{"kind":"ollama","baseURL":"http://localhost:11434","default":true}')
		ON CONFLICT DO NOTHING;
	`)
	if err != nil {
		pool.Close()
		t.Fatalf("seed provider: %v", err)
	}

	var mk []byte
	if withMasterKey {
		mk = make([]byte, 32)
		for i := range mk {
			mk[i] = byte(i + 1)
		}
	}

	store, err := configstore.PostgresFromPool(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatalf("configstore: %v", err)
	}
	store.SetMasterKey(mk)
	if err := store.Reload(ctx); err != nil {
		pool.Close()
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	deps := crudDeps(store.RawPool(), store)
	kinds := buildAdminKinds(store, nil)
	crudH := buildAdminCRUD(kinds, deps, store)
	mountAdminRoutes(r, adminTestToken, crudH, store, deps)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store
}

// --- Secret tests ---

func TestAdminSecret_EnvMode_CRUD(t *testing.T) {
	t.Setenv("RELAY_TEST_KEY", "test-value-1234")

	srv, store := buildSecretTestServer(t, false)

	body := map[string]any{
		"name":     "test-env-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind": "env",
			"env":  "RELAY_TEST_KEY",
		},
	}

	// Create → 201
	resp := adminReq(t, srv, http.MethodPost, "/admin/secrets", body)
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
	resp = adminReq(t, srv, http.MethodGet, "/admin/secrets/test-env-secret", nil)
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
	resp = adminReq(t, srv, http.MethodPut, "/admin/secrets/test-env-secret", updateBody)
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
	resp = adminReq(t, srv, http.MethodGet, "/admin/secrets/test-env-secret", nil)
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
	resp = adminReq(t, srv, http.MethodDelete, "/admin/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET after delete → 404
	resp = adminReq(t, srv, http.MethodGet, "/admin/secrets/test-env-secret", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminSecret_StoredMode_WithMasterKey(t *testing.T) {
	ctx := context.Background()
	srv, store := buildSecretTestServer(t, true)

	body := map[string]any{
		"name":     "test-stored-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-testapikey1234",
		},
	}

	// Create → 201
	resp := adminReq(t, srv, http.MethodPost, "/admin/secrets", body)
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
	row := store.RawPool().QueryRow(ctx, `SELECT value_ciphertext, value_from_env FROM secrets WHERE name='test-stored-secret'`)
	var ct []byte
	var envNull *string
	if err := row.Scan(&ct, &envNull); err != nil {
		t.Fatalf("scan pg row: %v", err)
	}
	if len(ct) == 0 {
		t.Error("want non-empty ciphertext in PG")
	}
	if envNull != nil {
		t.Error("want NULL value_from_env for stored-mode")
	}

	// GET → 200, value_masked present, no cleartext
	resp = adminReq(t, srv, http.MethodGet, "/admin/secrets/test-stored-secret", nil)
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
	resp = adminReq(t, srv, http.MethodPut, "/admin/secrets/test-stored-secret", updateBody)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update stored: want 200 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	row2 := store.RawPool().QueryRow(ctx, `SELECT value_ciphertext FROM secrets WHERE name='test-stored-secret'`)
	var ct2 []byte
	if err := row2.Scan(&ct2); err != nil {
		t.Fatalf("scan pg row after update: %v", err)
	}
	if string(ct2) == string(ct) {
		t.Error("ciphertext should change after update")
	}
}

func TestAdminSecret_StoredMode_NoMasterKey_400(t *testing.T) {
	srv, _ := buildSecretTestServer(t, false) // no master key

	body := map[string]any{
		"name":     "bad-secret",
		"provider": "seed-prov",
		"valueFrom": map[string]any{
			"kind":  "stored",
			"value": "sk-somevalue",
		},
	}

	resp := adminReq(t, srv, http.MethodPost, "/admin/secrets", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", resp.StatusCode)
	}
	var errOut map[string]map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errOut); err == nil {
		if errOut["error"]["code"] != "master_key_required" {
			t.Errorf("want code=master_key_required, got %q", errOut["error"]["code"])
		}
	}
}

func TestAdminSecret_DeleteReferenced_400(t *testing.T) {
	t.Setenv("RELAY_SEC_REF", "pool-ref-value")
	srv, _ := buildSecretTestServer(t, false)

	secBody := map[string]any{
		"name": "ref-secret", "provider": "seed-prov",
		"valueFrom": map[string]any{"kind": "env", "env": "RELAY_SEC_REF"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/admin/secrets", secBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create secret: want 201 got %d", resp.StatusCode)
	}

	poolBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Pool",
		"metadata": map[string]string{"name": "ref-pool"},
		"spec":     map[string]any{"provider": "seed-prov", "secrets": []string{"ref-secret"}},
	}
	resp = adminReq(t, srv, http.MethodPost, "/admin/pools", poolBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pool: want 201 got %d", resp.StatusCode)
	}

	resp = adminReq(t, srv, http.MethodDelete, "/admin/secrets/ref-secret", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete referenced secret: want 400 got %d", resp.StatusCode)
	}

	// Secret must still be present (no PG mutation happened).
	resp2 := adminReq(t, srv, http.MethodGet, "/admin/secrets/ref-secret", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("secret should still exist after rejected delete: got %d", resp2.StatusCode)
	}
}

func TestAdminSecret_List(t *testing.T) {
	t.Setenv("RELAY_LIST_KEY", "list-value")

	srv, _ := buildSecretTestServer(t, false)

	// Initially empty
	resp := adminReq(t, srv, http.MethodGet, "/admin/secrets", nil)
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
	resp = adminReq(t, srv, http.MethodPost, "/admin/secrets", secBody)
	resp.Body.Close()

	// List again
	resp = adminReq(t, srv, http.MethodGet, "/admin/secrets", nil)
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
// Attachments are created by PUTting a Pool with rateLimits inline.
func TestAdminAttachment_DerivedView(t *testing.T) {
	srv, store := buildSecretTestServer(t, false)

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
	resp := adminReq(t, srv, http.MethodPost, "/admin/ratelimits", rlBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create ratelimit: want 201 got %d", resp.StatusCode)
	}

	// Create a pool WITH inline rateLimits.
	poolBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Pool",
		"metadata": map[string]string{"name": "att-pool"},
		"spec": map[string]any{
			"provider":          "seed-prov",
			"skipDefaultLimits": true,
			"rateLimits":        []map[string]any{{"ref": "att-rl", "meter": "requests"}},
		},
	}
	resp = adminReq(t, srv, http.MethodPost, "/admin/pools", poolBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pool with rateLimits: want 201 got %d", resp.StatusCode)
	}

	// Verify snapshot reflects inline rateLimits.
	pool, ok := store.PoolByName("att-pool")
	if !ok {
		t.Fatal("pool not in snapshot")
	}
	if len(pool.Spec.RateLimits) == 0 {
		t.Error("pool spec missing RateLimits")
	} else if pool.Spec.RateLimits[0].Ref != "att-rl" {
		t.Errorf("want ref=att-rl, got %q", pool.Spec.RateLimits[0].Ref)
	}

	// GET /admin/attachments — all → includes our pool attachment.
	resp = adminReq(t, srv, http.MethodGet, "/admin/attachments", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list all: want 200 got %d", resp.StatusCode)
	}
	var listAll struct{ Items []map[string]any }
	decodeResp(t, resp, &listAll)
	found := false
	for _, item := range listAll.Items {
		if item["parentKind"] == "Pool" && item["parentName"] == "att-pool" && item["ratelimitName"] == "att-rl" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected attachment for att-pool in list-all response, got %+v", listAll.Items)
	}

	// GET /admin/attachments?parent_kind=Pool&parent_name=att-pool → filtered.
	resp = adminReq(t, srv, http.MethodGet, "/admin/attachments?parent_kind=Pool&parent_name=att-pool", nil)
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
	expectedID := "Pool:att-pool:att-rl:requests"
	if item["id"] != expectedID {
		t.Errorf("want id=%q, got %q", expectedID, item["id"])
	}

	// Removing the rateLimits via PUT removes it from the derived view.
	poolBodyNoRL := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Pool",
		"metadata": map[string]string{"name": "att-pool"},
		"spec": map[string]any{
			"provider":          "seed-prov",
			"skipDefaultLimits": true,
		},
	}
	resp = adminReq(t, srv, http.MethodPut, "/admin/pools/att-pool", poolBodyNoRL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update pool: want 200 got %d", resp.StatusCode)
	}

	resp = adminReq(t, srv, http.MethodGet, "/admin/attachments?parent_kind=Pool&parent_name=att-pool", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list after remove: want 200 got %d", resp.StatusCode)
	}
	var listEmpty struct{ Items []map[string]any }
	decodeResp(t, resp, &listEmpty)
	if len(listEmpty.Items) != 0 {
		t.Errorf("want 0 items after removing rateLimits, got %d", len(listEmpty.Items))
	}
}

// TestAdminAttachment_NoWriteEndpoints verifies that POST/DELETE /admin/attachments returns 404/405.
func TestAdminAttachment_NoWriteEndpoints(t *testing.T) {
	srv, _ := buildSecretTestServer(t, false)

	// POST /admin/attachments should now 404 (route is gone).
	attBody := map[string]any{"parentKind": "Pool", "parentName": "x", "ratelimitName": "y", "meter": "requests"}
	resp := adminReq(t, srv, http.MethodPost, "/admin/attachments", attBody)
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		t.Error("POST /admin/attachments should not succeed (write API removed)")
	}
}

// TestAdminAttachment_MissingQueryParams_400 verifies the filter validation.
func TestAdminAttachment_MissingQueryParams_400(t *testing.T) {
	srv, _ := buildSecretTestServer(t, false)

	resp := adminReq(t, srv, http.MethodGet, "/admin/attachments?parent_kind=Pool", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing parent_name: want 400 got %d", resp.StatusCode)
	}
}

// TestAdminSecret_OpenAPISchema_NoCleartextField verifies the response shape.
func TestAdminSecret_OpenAPISchema_NoCleartextField(t *testing.T) {
	srv, _ := buildSecretTestServer(t, false)

	resp := adminReq(t, srv, http.MethodGet, "/admin/secrets", nil)
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
