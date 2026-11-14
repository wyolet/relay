package gcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

const testEmail = "test-sa@test-project.iam.gserviceaccount.com"

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func saJSON(t *testing.T, email, keyID, keyPEM string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"type": "service_account", "client_email": email, "private_key_id": keyID, "private_key": keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decodeAssertion verifies an RS256 JWT against the test key and returns its header and claims.
func decodeAssertion(t *testing.T, assertion string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("assertion signature: %v", err)
	}
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("decode segment %d: %v", i, err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
	}
	return header, claims
}

// fakeEndpoint answers every request with status and body, keeping the requests and their bodies.
type fakeEndpoint struct {
	status int
	body   string
	err    error
	reqs   []*http.Request
	bodies []string
}

func (f *fakeEndpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, r)
	sent := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
	}
	f.bodies = append(f.bodies, sent)
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body)), Header: http.Header{}}, nil
}

func TestBuildJWTAssertion(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	wantClaims := map[string]any{
		"iss": testEmail, "scope": "https://www.googleapis.com/auth/cloud-platform", "aud": "https://oauth2.googleapis.com/token",
		"iat": float64(now.Unix()), "exp": float64(now.Unix() + 3600),
	}
	for _, tc := range []struct {
		keyID      string
		wantHeader map[string]any
	}{
		{"test-key-id", map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key-id"}},
		{"", map[string]any{"alg": "RS256", "typ": "JWT"}},
	} {
		assertion, err := buildJWTAssertion(serviceAccount{clientEmail: testEmail, privateKey: testKey(), privateKeyID: tc.keyID}, now)
		if err != nil {
			t.Fatal(err)
		}
		header, claims := decodeAssertion(t, assertion)
		if d := cmp.Diff(tc.wantHeader, header); d != "" {
			t.Errorf("kid %q: header (-want +got):\n%s", tc.keyID, d)
		}
		if d := cmp.Diff(wantClaims, claims); d != "" {
			t.Errorf("kid %q: claims (-want +got):\n%s", tc.keyID, d)
		}
	}
}

func TestTokenSource_ServiceAccountExchange(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())}))
	for _, tc := range []struct {
		name, keyPEM, endpoint, wantURL string
	}{
		{"pkcs8 key, default endpoint", pkcs8PEM(t, testKey()), "", "https://oauth2.googleapis.com/token"},
		{"pkcs1 key, configured endpoint", pkcs1, "http://localhost:9/token", "http://localhost:9/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEndpoint{status: 200, body: `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`}
			ts := NewTokenSource(Config{
				ServiceAccountJSON: saJSON(t, testEmail, "kid-1", tc.keyPEM), TokenEndpoint: tc.endpoint,
				HTTPClient: &http.Client{Transport: f}, Now: func() time.Time { return now },
			})
			tok, err := ts.AccessToken(context.Background())
			if err != nil || tok != "tok" {
				t.Fatalf("AccessToken = %q, %v", tok, err)
			}
			r := f.reqs[0]
			if r.Method != http.MethodPost || r.URL.String() != tc.wantURL || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("request = %s %s %v", r.Method, r.URL, r.Header)
			}
			form, err := url.ParseQuery(f.bodies[0])
			if err != nil || len(form) != 2 || form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Fatalf("form = %v, %v", form, err)
			}
			header, claims := decodeAssertion(t, form.Get("assertion"))
			if header["kid"] != "kid-1" || claims["iss"] != testEmail || claims["iat"] != float64(now.Unix()) {
				t.Errorf("assertion header %v claims %v", header, claims)
			}
		})
	}
}

func TestTokenSource_MetadataServerRequest(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, wantURL string
	}{
		{"default endpoint", "", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"},
		{"configured endpoint", "http://localhost:9/meta", "http://localhost:9/meta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEndpoint{status: 200, body: `{"access_token":"meta-token","expires_in":1800}`}
			// A service account is ignored once the metadata server is on.
			ts := NewTokenSource(Config{UseMetadataServer: true, ServiceAccountJSON: saJSON(t, testEmail, "", pkcs8PEM(t, testKey())), MetadataEndpoint: tc.endpoint, HTTPClient: &http.Client{Transport: f}})
			tok, err := ts.AccessToken(context.Background())
			if err != nil || tok != "meta-token" {
				t.Fatalf("AccessToken = %q, %v", tok, err)
			}
			r := f.reqs[0]
			if r.Method != http.MethodGet || r.URL.String() != tc.wantURL || r.Header.Get("Metadata-Flavor") != "Google" || f.bodies[0] != "" {
				t.Errorf("request = %s %s %v body %q", r.Method, r.URL, r.Header, f.bodies[0])
			}
		})
	}
}

func TestTokenSource_Caching(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		metadata   bool
		wantReqs   int
	}{
		{"an hour-long token is reused", `{"access_token":"tok","expires_in":3600}`, false, 1},
		{"no expires_in means an hour", `{"access_token":"tok"}`, false, 1},
		{"a token inside the refresh skew is not reused", `{"access_token":"tok","expires_in":59}`, false, 3},
		{"metadata: reused", `{"access_token":"tok","expires_in":1800}`, true, 1},
		{"metadata: no expires_in means an hour", `{"access_token":"tok","expires_in":0}`, true, 1},
		{"metadata: inside the refresh skew", `{"access_token":"tok","expires_in":59}`, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEndpoint{status: 200, body: tc.body}
			ts := NewTokenSource(Config{UseMetadataServer: tc.metadata, ServiceAccountJSON: saJSON(t, testEmail, "", pkcs8PEM(t, testKey())), HTTPClient: &http.Client{Transport: f}})
			for range 3 {
				if tok, err := ts.AccessToken(context.Background()); err != nil || tok != "tok" {
					t.Fatalf("AccessToken = %q, %v", tok, err)
				}
			}
			if len(f.reqs) != tc.wantReqs {
				t.Errorf("%d token requests for three calls, want %d", len(f.reqs), tc.wantReqs)
			}
		})
	}

	t.Run("expiry follows Config.Now", func(t *testing.T) {
		now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
		f := &fakeEndpoint{status: 200, body: `{"access_token":"tok","expires_in":3600}`}
		ts := NewTokenSource(Config{UseMetadataServer: true, HTTPClient: &http.Client{Transport: f}, Now: func() time.Time { return now }})
		for _, step := range []struct {
			advance  time.Duration
			wantReqs int
		}{{0, 1}, {59*time.Minute - time.Second, 1}, {time.Second, 2}, {2 * time.Hour, 3}} {
			now = now.Add(step.advance)
			if _, err := ts.AccessToken(context.Background()); err != nil || len(f.reqs) != step.wantReqs {
				t.Fatalf("at %s: %v after %d requests, want %d", now.Format(time.TimeOnly), err, len(f.reqs), step.wantReqs)
			}
		}
	})

	t.Run("service account: expiry follows Config.Now", func(t *testing.T) {
		now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
		f := &fakeEndpoint{status: 200, body: `{"access_token":"tok","expires_in":3600}`}
		ts := NewTokenSource(Config{ServiceAccountJSON: saJSON(t, testEmail, "", pkcs8PEM(t, testKey())), HTTPClient: &http.Client{Transport: f}, Now: func() time.Time { return now }})
		for _, step := range []struct {
			advance  time.Duration
			wantReqs int
		}{{0, 1}, {59*time.Minute - time.Second, 1}, {time.Second, 2}, {2 * time.Hour, 3}} {
			now = now.Add(step.advance)
			if _, err := ts.AccessToken(context.Background()); err != nil || len(f.reqs) != step.wantReqs {
				t.Fatalf("at %s: %v after %d requests, want %d", now.Format(time.TimeOnly), err, len(f.reqs), step.wantReqs)
			}
		}
	})
}

func TestTokenSource_Errors(t *testing.T) {
	down := errors.New("network down")
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := saJSON(t, testEmail, "", pkcs8PEM(t, testKey()))
	const notConfigured = "secret/gcp: service account credentials are not configured"
	for _, tc := range []struct {
		name     string
		cfg      Config
		f        fakeEndpoint
		want     string
		wantReqs int
	}{
		{"no service account", Config{}, fakeEndpoint{}, notConfigured, 0},
		{"service account is not json", Config{ServiceAccountJSON: []byte("{not json")}, fakeEndpoint{}, notConfigured, 0},
		{"no client_email", Config{ServiceAccountJSON: saJSON(t, "", "", pkcs8PEM(t, testKey()))}, fakeEndpoint{}, notConfigured, 0},
		{"no private_key", Config{ServiceAccountJSON: saJSON(t, testEmail, "", "")}, fakeEndpoint{}, notConfigured, 0},
		{"private_key is not pem", Config{ServiceAccountJSON: saJSON(t, testEmail, "", "not a pem block")}, fakeEndpoint{}, notConfigured, 0},
		{"private_key is not a key", Config{ServiceAccountJSON: saJSON(t, testEmail, "", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})))}, fakeEndpoint{}, notConfigured, 0},
		{"private_key is not rsa", Config{ServiceAccountJSON: saJSON(t, testEmail, "", pkcs8PEM(t, ec))}, fakeEndpoint{}, notConfigured, 0},
		{"endpoint does not parse", Config{ServiceAccountJSON: good, TokenEndpoint: "http://bad host"}, fakeEndpoint{}, `secret/gcp: build token request: parse "http://bad host": invalid character " " in host name`, 0},
		{"transport failure", Config{ServiceAccountJSON: good}, fakeEndpoint{err: down}, `secret/gcp: token request failed: Post "https://oauth2.googleapis.com/token": network down`, 1},
		{"error description", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 401, body: `{"error":"invalid_grant","error_description":"bad assertion"}`}, "secret/gcp: token exchange failed: bad assertion (HTTP 401)", 1},
		{"error only", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 400, body: `{"error":"invalid_grant"}`}, "secret/gcp: token exchange failed: invalid_grant (HTTP 400)", 1},
		{"json failure with no error field", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 500, body: ` {"detail":"x"} `}, `secret/gcp: token exchange failed: {"detail":"x"} (HTTP 500)`, 1},
		{"long failure body is capped", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 502, body: strings.Repeat("x", 511) + "é" + strings.Repeat("y", 100)}, "secret/gcp: token exchange failed: " + strings.Repeat("x", 511) + "… (HTTP 502)", 1},
		{"200 that is not json", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 200, body: `<html>`}, "secret/gcp: decode token response: invalid character '<' looking for beginning of value", 1},
		{"no access_token", Config{ServiceAccountJSON: good}, fakeEndpoint{status: 200, body: `{"expires_in":3600}`}, "secret/gcp: token response missing access_token", 1},
		{"metadata: endpoint does not parse", Config{UseMetadataServer: true, MetadataEndpoint: "http://bad host"}, fakeEndpoint{}, `secret/gcp: build metadata request: parse "http://bad host": invalid character " " in host name`, 0},
		{"metadata: transport failure", Config{UseMetadataServer: true, MetadataEndpoint: "http://meta.test/t"}, fakeEndpoint{err: down}, `secret/gcp: metadata token request failed: Get "http://meta.test/t": network down`, 1},
		{"metadata: refused", Config{UseMetadataServer: true}, fakeEndpoint{status: 403, body: `{"access_token":"must not be used"}`}, "secret/gcp: metadata server returned HTTP 403", 1},
		{"metadata: not json", Config{UseMetadataServer: true}, fakeEndpoint{status: 200, body: `<html>`}, "secret/gcp: decode metadata token: invalid character '<' looking for beginning of value", 1},
		{"metadata: no access_token", Config{UseMetadataServer: true}, fakeEndpoint{status: 200, body: `{"expires_in":3600}`}, "secret/gcp: metadata response missing access_token", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			tc.cfg.HTTPClient = &http.Client{Transport: &f}
			ts := NewTokenSource(tc.cfg)
			tok, err := ts.AccessToken(context.Background())
			if err == nil || err.Error() != tc.want || tok != "" || len(f.reqs) != tc.wantReqs {
				t.Fatalf("AccessToken = %q, %v after %d requests; want %q after %d", tok, err, len(f.reqs), tc.want, tc.wantReqs)
			}
			// A failure is not cached: the next call asks again.
			if _, err := ts.AccessToken(context.Background()); err == nil || len(f.reqs) != 2*tc.wantReqs {
				t.Errorf("second call: %v after %d requests, want %d", err, len(f.reqs), 2*tc.wantReqs)
			}
		})
	}

	t.Run("failure that is not json", func(t *testing.T) {
		f := &fakeEndpoint{status: 502, body: "bad gateway"}
		_, err := NewTokenSource(Config{ServiceAccountJSON: good, HTTPClient: &http.Client{Transport: f}}).AccessToken(context.Background())
		if want := "secret/gcp: token exchange failed: bad gateway (HTTP 502)"; err == nil || err.Error() != want {
			t.Fatalf("AccessToken: %v, want %q", err, want)
		}
	})
}
