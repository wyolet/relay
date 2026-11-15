package gcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/secret"
)

// fakeGoogle serves the token endpoints and Secret Manager from one transport, keyed by host, and keeps every request.
type fakeGoogle struct {
	token, secrets string // "<status> <body>"
	reqs           []*http.Request
}

func (f *fakeGoogle) RoundTrip(r *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, r)
	reply := f.token
	if strings.HasPrefix(r.URL.Path, "/v1/projects/") {
		reply = f.secrets
	}
	status, body, _ := strings.Cut(reply, " ")
	code := map[string]int{"200": 200, "401": 401, "404": 404, "500": 500}[status]
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func (f *fakeGoogle) config() Config {
	return Config{ProjectID: "test-project", UseMetadataServer: true, HTTPClient: &http.Client{Transport: f}}
}

const tokenOK = `200 {"access_token":"test-access-token","expires_in":3600,"token_type":"Bearer"}`

func TestResolver_Resolve(t *testing.T) {
	const base = "https://secretmanager.googleapis.com/v1/projects/test-project/secrets/"
	for _, tc := range []struct {
		name, path, secrets string
		wantURL             string
		want, wantErr       string
	}{
		{"latest by default", "openai-key", `200 {"name":"n","payload":{"data":"dXBzdHJlYW0tYXBpLWtleQ=="}}`, base + "openai-key/versions/latest:access", "upstream-api-key", ""},
		{"pinned version", "openai-key:7", `200 {"payload":{"data":"djc="}}`, base + "openai-key/versions/7:access", "v7", ""},
		{"only the first colon splits", "a:b:c", `200 {"payload":{"data":"djc="}}`, base + "a/versions/b:c:access", "v7", ""},
		{"name and version are escaped", "a b/c:d?e", `200 {"payload":{"data":"djc="}}`, base + "a%20b%2Fc/versions/d%3Fe:access", "v7", ""},
		{"not found", "missing", `404 {"error":{"code":404,"message":"Secret not found","status":"NOT_FOUND"}}`, base + "missing/versions/latest:access", "", "secret/gcp: Secret not found (HTTP 404)"},
		{"failure without a message", "x", `500 {"error":{"status":"INTERNAL"}}`, base + "x/versions/latest:access", "", "secret/gcp: secret manager returned HTTP 500"},
		{"opaque failure", "x", `500 <html>`, base + "x/versions/latest:access", "", "secret/gcp: secret manager returned HTTP 500"},
		{"200 that is not json", "x", `200 <html>`, base + "x/versions/latest:access", "", "secret/gcp: decode response: invalid character '<' looking for beginning of value"},
		{"empty payload", "x", `200 {"payload":{"data":""}}`, base + "x/versions/latest:access", "", `secret/gcp: response for "x" has empty payload`},
		{"no payload", "x", `200 {"name":"n"}`, base + "x/versions/latest:access", "", `secret/gcp: response for "x" has empty payload`},
		{"payload is not base64", "x", `200 {"payload":{"data":"!!!"}}`, base + "x/versions/latest:access", "", "secret/gcp: decode payload: illegal base64 data at input byte 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGoogle{token: tokenOK, secrets: tc.secrets}
			got, err := New(f.config()).Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: tc.path})
			if string(got) != tc.want || (tc.wantErr == "") != (err == nil) || (err != nil && err.Error() != tc.wantErr) {
				t.Fatalf("Resolve = %q, %v; want %q, %q", got, err, tc.want, tc.wantErr)
			}
			if len(f.reqs) != 2 {
				t.Fatalf("%d requests, want a token request then a secret request", len(f.reqs))
			}
			meta, s := f.reqs[0], f.reqs[1]
			if meta.URL.String() != "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" || meta.Header.Get("Metadata-Flavor") != "Google" {
				t.Errorf("token request = %s %v", meta.URL, meta.Header)
			}
			if s.Method != http.MethodGet || s.URL.String() != tc.wantURL || s.Header.Get("Authorization") != "Bearer test-access-token" {
				t.Errorf("secret request = %s %s auth %q, want GET %s", s.Method, s.URL, s.Header.Get("Authorization"), tc.wantURL)
			}
		})
	}
}

func TestResolver_ServiceAccountAndEndpoints(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	sa, err := json.Marshal(map[string]string{"client_email": "sa@test-project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGoogle{token: tokenOK, secrets: `200 {"payload":{"data":"djc="}}`}
	res := New(Config{
		ProjectID: "my project", ServiceAccountJSON: sa, HTTPClient: &http.Client{Transport: f},
		TokenEndpoint: "https://login.test/token", SecretManagerEndpoint: "https://sm.test//",
	})
	for range 2 {
		if got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindGCP, Path: "s"}); err != nil || string(got) != "v7" {
			t.Fatalf("Resolve = %q, %v", got, err)
		}
	}
	var urls []string
	for _, r := range f.reqs {
		urls = append(urls, r.Method+" "+r.URL.String())
	}
	// One exchange serves both reads.
	want := []string{"POST https://login.test/token", "GET https://sm.test/v1/projects/my%20project/secrets/s/versions/latest:access", "GET https://sm.test/v1/projects/my%20project/secrets/s/versions/latest:access"}
	if d := cmp.Diff(want, urls); d != "" {
		t.Errorf("requests (-want +got):\n%s", d)
	}
}

func TestResolver_RefusesBeforeSecretManager(t *testing.T) {
	gcp := func(path string) secret.Ref { return secret.Ref{Kind: secret.KindGCP, Path: path} }
	for _, tc := range []struct {
		name     string
		ref      secret.Ref
		cfg      func(*Config)
		token    string
		want     string
		wantReqs int
	}{
		{"wrong kind", secret.Ref{Kind: secret.KindEnv, Path: "x"}, nil, tokenOK, `secret/gcp: wrong kind "env"`, 0},
		{"empty path", gcp(""), nil, tokenOK, "secret/gcp: path is empty", 0},
		{"no name", gcp(":onlyversion"), nil, tokenOK, `secret/gcp: path ":onlyversion": missing secret name before ":"`, 0},
		{"no version", gcp("name:"), nil, tokenOK, `secret/gcp: path "name:": missing version after ":"`, 0},
		{"no project", gcp("x"), func(c *Config) { c.ProjectID = " " }, tokenOK, "secret/gcp: project is not configured", 0},
		{"no credentials", gcp("x"), func(c *Config) { c.UseMetadataServer = false }, tokenOK, "secret/gcp: service account credentials are not configured", 0},
		{"token refused", gcp("x"), nil, `401 {}`, "secret/gcp: metadata server returned HTTP 401", 1},
		{"endpoint does not parse", gcp("x"), func(c *Config) { c.SecretManagerEndpoint = "https://bad host" }, tokenOK, `secret/gcp: build request: parse "https://bad host/v1/projects/test-project/secrets/x/versions/latest:access": invalid character " " in host name`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGoogle{token: tc.token, secrets: `200 {"payload":{"data":"bXVzdCBub3QgYmUgcmVhZA=="}}`}
			cfg := f.config()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			got, err := New(cfg).Resolve(context.Background(), tc.ref)
			if err == nil || err.Error() != tc.want || got != nil || len(f.reqs) != tc.wantReqs {
				t.Errorf("Resolve = %q, %v after %d requests; want %q after %d", got, err, len(f.reqs), tc.want, tc.wantReqs)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	const inline, onDisk = `{"project_id":"sa-project","client_email":"inline"}`, `{"client_email":"on-disk"}`
	file := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(file, []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent.json")
	const noProject, noCreds = "secret/gcp: GCP_PROJECT is unset or empty", "secret/gcp: GCP_SA_JSON or GOOGLE_APPLICATION_CREDENTIALS is required"

	for _, tc := range []struct {
		name string
		env  [5]string // GCP_PROJECT, GOOGLE_CLOUD_PROJECT, GCP_USE_METADATA_SERVER, GCP_SA_JSON, GOOGLE_APPLICATION_CREDENTIALS
		want Config
		err  string
	}{
		{"inline service account", [5]string{" my-project ", "", "", " " + inline + "\n", ""}, Config{ProjectID: "my-project", ServiceAccountJSON: []byte(inline)}, ""},
		{"GCP_PROJECT wins", [5]string{"my-project", "cloud-project", "", inline, ""}, Config{ProjectID: "my-project", ServiceAccountJSON: []byte(inline)}, ""},
		{"GOOGLE_CLOUD_PROJECT is the fallback", [5]string{" ", "cloud-project", "", inline, ""}, Config{ProjectID: "cloud-project", ServiceAccountJSON: []byte(inline)}, ""},
		{"project from the service account", [5]string{"", "", "", inline, ""}, Config{ProjectID: "sa-project", ServiceAccountJSON: []byte(inline)}, ""},
		{"service account file", [5]string{"my-project", "", "", "", " " + file + " "}, Config{ProjectID: "my-project", ServiceAccountJSON: []byte(onDisk)}, ""},
		{"inline wins over the file", [5]string{"my-project", "", "", inline, missing}, Config{ProjectID: "my-project", ServiceAccountJSON: []byte(inline)}, ""},
		{"metadata server drops the service account", [5]string{"", "cloud-project", "1", inline, file}, Config{ProjectID: "cloud-project", UseMetadataServer: true}, ""},
		{"metadata server needs a project", [5]string{"", "", "1", inline, ""}, Config{}, noProject},
		{"only 1 turns the metadata server on", [5]string{"my-project", "", "true", "", ""}, Config{}, noCreds},
		{"no credentials", [5]string{"my-project", "", "", " ", " "}, Config{}, noCreds},
		{"empty file", [5]string{"my-project", "", "", "", os.DevNull}, Config{}, noCreds},
		{"file without a project", [5]string{"", "", "", "", file}, Config{}, noProject},
		{"service account that is not json, no project", [5]string{"", "", "", "{not json", ""}, Config{}, noProject},
		{"file does not exist", [5]string{"my-project", "", "", "", missing}, Config{}, "secret/gcp: read GOOGLE_APPLICATION_CREDENTIALS: open " + missing + ": no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, name := range []string{"GCP_PROJECT", "GOOGLE_CLOUD_PROJECT", "GCP_USE_METADATA_SERVER", "GCP_SA_JSON", "GOOGLE_APPLICATION_CREDENTIALS"} {
				t.Setenv(name, tc.env[i])
			}
			cfg, err := ConfigFromEnv()
			if d := cmp.Diff(tc.want, cfg); d != "" || (tc.err == "") != (err == nil) || (err != nil && err.Error() != tc.err) {
				t.Errorf("ConfigFromEnv: %v, want %q (-want +got):\n%s", err, tc.err, d)
			}
		})
	}
}
