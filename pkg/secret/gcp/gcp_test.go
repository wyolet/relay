package gcp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

func TestParsePath(t *testing.T) {
	t.Parallel()

	name, ver, err := parsePath("openai-key")
	if err != nil || name != "openai-key" || ver != "latest" {
		t.Fatalf("latest default: name=%q ver=%q err=%v", name, ver, err)
	}

	name, ver, err = parsePath("openai-key:3")
	if err != nil || name != "openai-key" || ver != "3" {
		t.Fatalf("version 3: name=%q ver=%q err=%v", name, ver, err)
	}

	name, ver, err = parsePath("openai-key:latest")
	if err != nil || name != "openai-key" || ver != "latest" {
		t.Fatalf("explicit latest: name=%q ver=%q err=%v", name, ver, err)
	}

	for _, p := range []string{"", ":onlyversion", "name:"} {
		if _, _, err := parsePath(p); err == nil {
			t.Fatalf("parsePath(%q): want error", p)
		}
	}
}

func TestBuildJWTAssertion(t *testing.T) {
	t.Parallel()

	key, sa := testServiceAccount(t)
	fixed := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	assertion, err := buildJWTAssertion(sa, fixed)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts: got %d", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" || header["kid"] != "test-key-id" {
		t.Fatalf("header: %+v", header)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != sa.clientEmail {
		t.Fatalf("iss: %+v", claims)
	}
	if claims["scope"] != cloudPlatformScope {
		t.Fatalf("scope: %+v", claims)
	}
	if claims["aud"] != tokenAudience {
		t.Fatalf("aud: %+v", claims)
	}
	if int64(claims["iat"].(float64)) != fixed.Unix() {
		t.Fatalf("iat: %+v", claims)
	}
	if int64(claims["exp"].(float64)) != fixed.Unix()+3600 {
		t.Fatalf("exp: %+v", claims)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sigBytes); err != nil {
		t.Fatalf("signature invalid: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("GCP_PROJECT", "my-project")
	t.Setenv("GCP_SA_JSON", testSAJSON(t))

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectID != "my-project" || cfg.UseMetadataServer {
		t.Fatalf("cfg: %+v", cfg)
	}
	if len(cfg.ServiceAccountJSON) == 0 {
		t.Fatal("missing sa json")
	}

	t.Setenv("GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "cloud-project")
	t.Setenv("GCP_USE_METADATA_SERVER", "1")
	t.Setenv("GCP_SA_JSON", "")

	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UseMetadataServer || cfg.ProjectID != "cloud-project" {
		t.Fatalf("metadata cfg: %+v", cfg)
	}
}

func TestResolver_LatestVersion(t *testing.T) {
	const want = "upstream-api-key"
	var seenAuth, seenPath string

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != jwtGrantType || r.Form.Get("assertion") == "" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "test-access-token",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		if !strings.HasSuffix(r.URL.Path, "/versions/latest:access") {
			http.Error(w, "wrong version path", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(accessSecretResponse{
			Name: "projects/test-project/secrets/openai-key/versions/latest",
			Payload: struct {
				Data string `json:"data"`
			}{Data: base64.StdEncoding.EncodeToString([]byte(want))},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := New(testConfig(t, srv))
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: "openai-key"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if seenAuth != "Bearer test-access-token" {
		t.Fatalf("auth header: %q", seenAuth)
	}
	if !strings.Contains(seenPath, "/secrets/openai-key/versions/latest:access") {
		t.Fatalf("path: %q", seenPath)
	}
}

func TestResolver_SpecificVersion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/versions/7:access") {
			http.Error(w, "wrong version", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(accessSecretResponse{
			Payload: struct {
				Data string `json:"data"`
			}{Data: base64.StdEncoding.EncodeToString([]byte("v7"))},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := New(testConfig(t, srv))
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: "openai-key:7"})
	if err != nil || string(got) != "v7" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_MetadataServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metadata/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != metadataFlavor {
			http.Error(w, "missing flavor", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "meta-token", ExpiresIn: 1800})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer meta-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(accessSecretResponse{
			Payload: struct {
				Data string `json:"data"`
			}{Data: base64.StdEncoding.EncodeToString([]byte("from-meta"))},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		ProjectID:             "test-project",
		UseMetadataServer:     true,
		HTTPClient:            http.DefaultClient,
		MetadataEndpoint:      srv.URL + "/metadata/token",
		SecretManagerEndpoint: srv.URL,
	}
	res := New(cfg)
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: "key"})
	if err != nil || string(got) != "from-meta" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_TokenCaching(t *testing.T) {
	tokenCalls := 0
	secretCalls := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "cached-token", ExpiresIn: 3600})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		secretCalls++
		_ = json.NewEncoder(w).Encode(accessSecretResponse{
			Payload: struct {
				Data string `json:"data"`
			}{Data: base64.StdEncoding.EncodeToString([]byte("x"))},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testConfig(t, srv)
	fixed := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	cfg.Now = func() time.Time { return fixed }
	res := New(cfg)

	ref := secret.Ref{Kind: secret.KindGCP, Path: "s"}
	for i := 0; i < 2; i++ {
		if _, err := res.Resolve(context.Background(), ref); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls: got %d want 1", tokenCalls)
	}
	if secretCalls != 2 {
		t.Fatalf("secret calls: got %d want 2", secretCalls)
	}
}

func TestResolver_Errors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Secret not found","status":"NOT_FOUND"}}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := New(testConfig(t, srv))

	cases := []struct {
		ref secret.Ref
		sub string
	}{
		{secret.Ref{Kind: secret.KindEnv, Path: "x"}, "wrong kind"},
		{secret.Ref{Kind: secret.KindGCP, Path: "missing"}, "Secret not found"},
	}
	for _, tc := range cases {
		_, err := res.Resolve(context.Background(), tc.ref)
		if err == nil || !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("ref %+v: got err %v want %q", tc.ref, err, tc.sub)
		}
	}
}

func TestResolver_Non2xxToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad assertion"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := New(testConfig(t, srv))
	_, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: "s"})
	if err == nil || !strings.Contains(err.Error(), "bad assertion") {
		t.Fatalf("token error: %v", err)
	}
}

func testConfig(t *testing.T, srv *httptest.Server) Config {
	t.Helper()
	return Config{
		ProjectID:             "test-project",
		ServiceAccountJSON:    []byte(testSAJSON(t)),
		HTTPClient:            http.DefaultClient,
		TokenEndpoint:         srv.URL + "/token",
		SecretManagerEndpoint: srv.URL,
		Now: func() time.Time {
			return time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
		},
	}
}

func testServiceAccount(t *testing.T) (*rsa.PrivateKey, serviceAccount) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, serviceAccount{
		clientEmail:  "test-sa@test-project.iam.gserviceaccount.com",
		privateKey:   key,
		privateKeyID: "test-key-id",
	}
}

func testSAJSON(t *testing.T) string {
	t.Helper()
	key, _ := testServiceAccount(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	doc := map[string]string{
		"type":                        "service_account",
		"project_id":                  "test-project",
		"private_key_id":              "test-key-id",
		"private_key":                 string(pemBlock),
		"client_email":                "test-sa@test-project.iam.gserviceaccount.com",
		"client_id":                   "123",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
