package oauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jws"

	"github.com/wyolet/relay/sdk/oauth"
)

var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func withClient(srv *httptest.Server) context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, srv.Client())
}

func grant(t *testing.T, tokenURL string) oauth.JWTBearer {
	t.Helper()
	return oauth.JWTBearer{
		Issuer:       "svc@example.test",
		PrivateKey:   pkcs8PEM(t, testRSAKey()),
		PrivateKeyID: "kid-1",
		TokenURL:     tokenURL,
		Scopes:       []string{"scope-a", "scope-b"},
	}
}

func TestJWTBearerTokenPostsSignedAssertion(t *testing.T) {
	var form map[string][]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	for _, tc := range []struct{ name, audience, wantAud string }{
		{"audience defaults to the token URL", "", srv.URL + "/token"},
		{"explicit audience", "https://aud.example.test", "https://aud.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := grant(t, srv.URL+"/token")
			g.Audience = tc.audience
			tok, err := g.Token(withClient(srv))
			if err != nil {
				t.Fatalf("Token: %v", err)
			}
			if tok.AccessToken != "minted" || tok.Expiry.Before(time.Now().Add(50*time.Minute)) {
				t.Fatalf("token: access %q expiry %v", tok.AccessToken, tok.Expiry)
			}
			if got := form["grant_type"]; len(got) != 1 || got[0] != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Fatalf("grant_type = %v", got)
			}
			assertion := form["assertion"]
			if len(assertion) != 1 {
				t.Fatalf("assertion = %v", assertion)
			}
			if err := jws.Verify(assertion[0], &testRSAKey().PublicKey); err != nil {
				t.Fatalf("assertion does not verify against the key: %v", err)
			}
			claims, err := jws.Decode(assertion[0])
			if err != nil {
				t.Fatal(err)
			}
			if claims.Iss != "svc@example.test" || claims.Aud != tc.wantAud || claims.Scope != "scope-a scope-b" {
				t.Fatalf("claims: iss %q aud %q scope %q", claims.Iss, claims.Aud, claims.Scope)
			}
			head, err := base64.RawURLEncoding.DecodeString(strings.Split(assertion[0], ".")[0])
			if err != nil {
				t.Fatal(err)
			}
			var hdr struct {
				KeyID string `json:"kid"`
			}
			if err := json.Unmarshal(head, &hdr); err != nil || hdr.KeyID != "kid-1" {
				t.Fatalf("header %s: %v", head, err)
			}
		})
	}
}

func TestJWTBearerRejectionCarriesNoBody(t *testing.T) {
	for _, tc := range []struct{ name, body, wantCode string }{
		{"rfc error fields", `{"error":"invalid_grant","error_description":"bad signature","echo":"BODY-ECHO"}`, "invalid_grant"},
		{"no error code", `BODY-ECHO`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := grant(t, srv.URL).Token(withClient(srv))
			var re *oauth2.RetrieveError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *oauth2.RetrieveError", err)
			}
			if re.Response.StatusCode != http.StatusBadRequest || re.ErrorCode != tc.wantCode || len(re.Body) != 0 {
				t.Fatalf("status %d code %q body %q", re.Response.StatusCode, re.ErrorCode, re.Body)
			}
			if strings.Contains(err.Error(), "BODY-ECHO") {
				t.Fatalf("error carries the response body: %v", err)
			}
		})
	}
}

func TestJWTBearerTokenReturnsByContextDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(withClient(srv), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := grant(t, srv.URL).Token(ctx); err == nil {
		t.Fatal("Token against a hanging endpoint returned no error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Token returned after %v, want about the 200ms deadline", took)
	}
}

func TestParseCredential(t *testing.T) {
	rsaPEM := pkcs8PEM(t, testRSAKey())
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := func(mut func(map[string]any)) []byte {
		m := map[string]any{
			"type":           "service_account",
			"client_email":   "svc@example.test",
			"private_key":    rsaPEM,
			"private_key_id": "kid-1",
			"token_uri":      "https://token.example.test/token",
			"scopes":         []string{"scope-a"},
		}
		if mut != nil {
			mut(m)
		}
		b, _ := json.Marshal(m)
		return b
	}

	t.Run("token blob", func(t *testing.T) {
		c, err := oauth.ParseCredential([]byte(`{"access_token":"at","refresh_token":"rt","expiry":"2099-01-01T00:00:00Z"}`))
		if err != nil || c.JWTBearer != nil || c.AccessToken != "at" || c.RefreshToken != "rt" {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("credential shape", func(t *testing.T) {
		raw, _ := json.Marshal(oauth.Credential{
			Token:     oauth.Token{AccessToken: "at"},
			JWTBearer: &oauth.JWTBearer{Issuer: "svc@example.test", PrivateKey: pkcs1PEM(testRSAKey()), TokenURL: "https://token.example.test", Scopes: []string{"s"}},
		})
		c, err := oauth.ParseCredential(raw)
		if err != nil || c.JWTBearer == nil || c.JWTBearer.Issuer != "svc@example.test" || c.AccessToken != "at" {
			t.Fatalf("got %+v, %v", c, err)
		}
	})
	t.Run("service-account key file", func(t *testing.T) {
		c, err := oauth.ParseCredential(keyFile(nil))
		if err != nil {
			t.Fatal(err)
		}
		g := c.JWTBearer
		if g == nil || g.Issuer != "svc@example.test" || g.PrivateKey != rsaPEM || g.PrivateKeyID != "kid-1" ||
			g.TokenURL != "https://token.example.test/token" || len(g.Scopes) != 1 || c.AccessToken != "" {
			t.Fatalf("got %+v", c)
		}
	})

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"key file without scopes", keyFile(func(m map[string]any) { delete(m, "scopes") })},
		{"non-RSA key", keyFile(func(m map[string]any) { m["private_key"] = pkcs8PEM(t, ecKey) })},
		{"http token URL", keyFile(func(m map[string]any) { m["token_uri"] = "http://token.example.test/token" })},
		{"no token and no grant", []byte(`{"token_type":"Bearer"}`)},
		{"not JSON", []byte(`{"private_key":"VALUE-MARKER`)},
		{"field of the wrong type", []byte(`{"type":"service_account","scopes":"VALUE-MARKER"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := oauth.ParseCredential(tc.raw)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "VALUE-MARKER") {
				t.Fatalf("error quotes the value: %v", err)
			}
		})
	}
}
