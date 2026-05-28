package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

func TestParsePath(t *testing.T) {
	name, ver, err := parsePath("openai-key/abc123")
	if err != nil || name != "openai-key" || ver != "abc123" {
		t.Fatalf("versioned: %q %q err %v", name, ver, err)
	}
	name, ver, err = parsePath("openai-key")
	if err != nil || name != "openai-key" || ver != "" {
		t.Fatalf("plain: %q %q err %v", name, ver, err)
	}
	for _, p := range []string{"", "/onlyver", "name/"} {
		if _, _, err := parsePath(p); err == nil {
			t.Fatalf("parsePath(%q): want error", p)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("AZURE_KEYVAULT_URL", "https://myvault.vault.azure.net")
	t.Setenv("AZURE_TENANT_ID", "tenant-id")
	t.Setenv("AZURE_CLIENT_ID", "client-id")
	t.Setenv("AZURE_CLIENT_SECRET", "client-secret")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VaultURL != "https://myvault.vault.azure.net" ||
		cfg.TenantID != "tenant-id" ||
		cfg.ClientID != "client-id" ||
		cfg.ClientSecret != "client-secret" ||
		cfg.UseManagedIdentity {
		t.Fatalf("cfg: %+v", cfg)
	}
}

func TestConfigFromEnv_ManagedIdentity(t *testing.T) {
	t.Setenv("AZURE_KEYVAULT_URL", "https://myvault.vault.azure.net")
	t.Setenv("AZURE_USE_MANAGED_IDENTITY", "1")
	t.Setenv("AZURE_TENANT_ID", "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UseManagedIdentity || cfg.TenantID != "" {
		t.Fatalf("cfg: %+v", cfg)
	}
}

func TestResolver_PlainSecret(t *testing.T) {
	var tokenCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil ||
			r.Form.Get("grant_type") != "client_credentials" ||
			r.Form.Get("scope") != vaultScope {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "access-tok",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer tokenSrv.Close()

	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.URL.Path, "/secrets/openai-key") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("api-version") != apiVersion {
			http.Error(w, "bad api-version", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(secretBundle{Value: "upstream-api-key"})
	}))
	defer vaultSrv.Close()

	res := New(testConfig(tokenSrv.URL, vaultSrv.URL, nil))
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "openai-key"})
	if err != nil || string(got) != "upstream-api-key" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls: %d", tokenCalls.Load())
	}

	got2, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "openai-key"})
	if err != nil || string(got2) != "upstream-api-key" {
		t.Fatalf("cached resolve: %q err %v", got2, err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("cached token calls: %d", tokenCalls.Load())
	}
}

func TestResolver_VersionedSecret(t *testing.T) {
	tokenSrv := httptest.NewServer(tokenOKHandler("tok"))
	defer tokenSrv.Close()

	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/secrets/my-secret/ver-42") {
			http.Error(w, "path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(secretBundle{Value: "versioned-value"})
	}))
	defer vaultSrv.Close()

	res := New(testConfig(tokenSrv.URL, vaultSrv.URL, nil))
	got, err := res.Resolve(context.Background(), secret.Ref{
		Kind: secret.KindAzure,
		Path: "my-secret/ver-42",
	})
	if err != nil || string(got) != "versioned-value" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_MissingSecret(t *testing.T) {
	tokenSrv := httptest.NewServer(tokenOKHandler("tok"))
	defer tokenSrv.Close()

	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"SecretNotFound","message":"Secret not found"}}`))
	}))
	defer vaultSrv.Close()

	res := New(testConfig(tokenSrv.URL, vaultSrv.URL, nil))
	_, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "missing"})
	if err == nil || !strings.Contains(err.Error(), "Secret not found") {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestResolver_TokenError(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad creds"}`))
	}))
	defer tokenSrv.Close()

	vaultSrv := httptest.NewServer(http.NotFoundHandler())
	defer vaultSrv.Close()

	res := New(testConfig(tokenSrv.URL, vaultSrv.URL, nil))
	_, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("token error: %v", err)
	}
}

func TestResolver_TokenRefresh(t *testing.T) {
	var tokenCalls atomic.Int32
	now := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := func() time.Time { return now }

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := tokenCalls.Add(1)
		tok := "tok-1"
		if n > 1 {
			tok = "tok-2"
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: tok,
			ExpiresIn:   120,
			TokenType:   "Bearer",
		})
	}))
	defer tokenSrv.Close()

	var seenAuth atomic.Value
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(secretBundle{Value: "v"})
	}))
	defer vaultSrv.Close()

	cfg := testConfig(tokenSrv.URL, vaultSrv.URL, clock)
	res := New(cfg)

	if _, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "s"}); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("first token calls: %d", tokenCalls.Load())
	}

	now = now.Add(2 * time.Hour)
	if _, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "s"}); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("refresh token calls: %d", tokenCalls.Load())
	}
	if auth, _ := seenAuth.Load().(string); auth != "Bearer tok-2" {
		t.Fatalf("refreshed auth: %q", auth)
	}
}

func TestResolver_ManagedIdentity(t *testing.T) {
	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Metadata") != "true" {
			http.Error(w, "missing metadata", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("resource") != vaultResource {
			http.Error(w, "bad resource", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "mi-tok",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}))
	defer imdsSrv.Close()

	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mi-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(secretBundle{Value: "mi-secret"})
	}))
	defer vaultSrv.Close()

	res := New(Config{
		VaultURL:           vaultSrv.URL,
		UseManagedIdentity: true,
		IMDSEndpoint: imdsSrv.URL + "?api-version=2019-08-01&resource=" + url.QueryEscape(vaultResource),
		HTTPClient:         http.DefaultClient,
	})
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "key"})
	if err != nil || string(got) != "mi-secret" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_Errors(t *testing.T) {
	tokenSrv := httptest.NewServer(tokenOKHandler("tok"))
	defer tokenSrv.Close()
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(secretBundle{Value: "x"})
	}))
	defer vaultSrv.Close()

	res := New(testConfig(tokenSrv.URL, vaultSrv.URL, nil))
	_, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindEnv, Path: "x"})
	if err == nil || !strings.Contains(err.Error(), "wrong kind") {
		t.Fatalf("wrong kind: %v", err)
	}
}

func tokenOKHandler(tok string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: tok,
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	}
}

func testConfig(tokenURL, vaultURL string, now func() time.Time) Config {
	return Config{
		VaultURL:      vaultURL,
		TenantID:      "tenant",
		ClientID:      "client",
		ClientSecret:  "secret",
		TokenEndpoint: tokenURL,
		HTTPClient:    http.DefaultClient,
		Now:           now,
	}
}
