package gcp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

func TestTokenSource_MetadataServerCachesToken(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Metadata-Flavor") != metadataFlavor {
			http.Error(w, "missing flavor", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "meta-token", ExpiresIn: 1800})
	}))
	defer srv.Close()

	ts := NewTokenSource(Config{UseMetadataServer: true, MetadataEndpoint: srv.URL, HTTPClient: http.DefaultClient})
	for i := 0; i < 2; i++ {
		tok, err := ts.AccessToken(context.Background())
		if err != nil || tok != "meta-token" {
			t.Fatalf("token %d: %q err %v", i, tok, err)
		}
	}
	if calls != 1 {
		t.Fatalf("metadata calls: got %d want 1", calls)
	}
}

func TestTokenSource_UnparsableServiceAccount(t *testing.T) {
	ts := NewTokenSource(Config{ServiceAccountJSON: []byte("{not json")})
	_, err := ts.AccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "service account credentials are not configured") {
		t.Fatalf("got %v", err)
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
