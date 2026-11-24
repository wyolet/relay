//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/internal/storage/storagetest"
	pkgoauth "github.com/wyolet/relay/pkg/secret/oauth"
)

// seedKeyFileTree is a tree declaring a host, its account and an oauth host key holding a service-account key file whose token URL is tokenURI.
func seedKeyFileTree(t *testing.T, keyName, tokenURI string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	keyFile, err := json.Marshal(map[string]any{
		"type": "service_account", "client_email": "svc@example.test", "private_key_id": "kid-1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   tokenURI, "scopes": []string{"scope-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tree := fmt.Sprintf(`apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata: {name: sa-host}
spec: {baseURL: "https://upstream.invalid"}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata: {name: sa-org}
spec: {host: sa-host}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostKey
metadata: {name: %s}
spec:
  hostId: sa-host
  account: sa-org
  valueFrom: {kind: oauth, provider: service-account}
  value: '%s'
`, keyName, keyFile)
	if err := os.WriteFile(filepath.Join(dir, "tree.yaml"), []byte(tree), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// `relay seed --apply` checks a service-account key file against RELAY_OAUTH_TOKEN_HOSTS as the control API does: a listed token host is exchanged and stored, an unlisted one is refused naming the setting.
func TestIntegration_SeedApplyChecksOAuthTokenHosts(t *testing.T) {
	tokenEndpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenEndpoint.Close()
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	t.Setenv("RELAY_OAUTH_TOKEN_HOSTS", "127.0.0.1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool := storagetest.Pool(t)
	// The token endpoint is a local TLS test server, which the exchange client refuses unless told otherwise.
	sec, err := cliSecrets(pool, cfg, pkgoauth.AllowLoopback(tokenEndpoint.Client().Transport.(*http.Transport).TLSClientConfig))
	if err != nil {
		t.Fatal(err)
	}

	res, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: seedKeyFileTree(t, "sa-listed", tokenEndpoint.URL+"/token"), Secrets: sec})
	if err != nil || res.HostKeys != 1 {
		t.Fatalf("seed --apply with a listed token host: %+v, %v; want the host key written", res, err)
	}

	_, err = seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: seedKeyFileTree(t, "sa-unlisted", "https://token.example.test/token"), Secrets: sec})
	if msg := fmt.Sprint(err); !strings.Contains(msg, "RELAY_OAUTH_TOKEN_HOSTS") || !strings.Contains(msg, "token.example.test") || strings.Contains(msg, "PRIVATE KEY") {
		t.Fatalf("seed --apply with an unlisted token host: err = %v, want a refusal naming the host and RELAY_OAUTH_TOKEN_HOSTS", err)
	}

	rows, err := apply.Load(ctx, apply.NewStores(pool, sec))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range rows.HostKeys {
		names = append(names, k.Meta.Name)
	}
	if len(names) != 1 || names[0] != "sa-listed" {
		t.Fatalf("host keys = %v, want only sa-listed", names)
	}
}
