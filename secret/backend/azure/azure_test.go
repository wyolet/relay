package azure

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/secret"
)

// fakeAzure serves the token endpoint and the vault from one transport, keyed by host, and keeps every request.
type fakeAzure struct {
	token, vault string // "<status> <body>"
	reqs         []*http.Request
	forms        []url.Values
}

func (f *fakeAzure) RoundTrip(r *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, r)
	reply := f.vault
	if r.URL.Host != "vault.test" {
		reply = f.token
		var form url.Values
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			form, _ = url.ParseQuery(string(b))
		}
		f.forms = append(f.forms, form)
	}
	status, body, _ := strings.Cut(reply, " ")
	code := map[string]int{"200": 200, "401": 401, "404": 404, "500": 500}[status]
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func (f *fakeAzure) config() Config {
	return Config{
		VaultURL: " https://vault.test/ ", TenantID: "tenant", ClientID: "client", ClientSecret: "secret",
		TokenEndpoint: "https://login.test/token", HTTPClient: &http.Client{Transport: f},
	}
}

const tokenOK = `200 {"access_token":"access-tok","expires_in":3600,"token_type":"Bearer"}`

func TestResolver_Resolve(t *testing.T) {
	for _, tc := range []struct {
		name, path, vault string
		wantURL           string
		want, wantErr     string
	}{
		{"latest version", "openai-key", `200 {"value":"upstream-api-key","id":"x"}`, "https://vault.test/secrets/openai-key?api-version=2025-07-01", "upstream-api-key", ""},
		{"pinned version", " my-secret/ver-42 ", `200 {"value":"versioned-value"}`, "https://vault.test/secrets/my-secret/ver-42?api-version=2025-07-01", "versioned-value", ""},
		{"name and version are escaped", "a b?/c#d/e", `200 {"value":"v"}`, "https://vault.test/secrets/a%20b%3F/c%23d%2Fe?api-version=2025-07-01", "v", ""},
		{"not found", "missing", `404 {"error":{"code":"SecretNotFound","message":"Secret not found"}}`, "https://vault.test/secrets/missing?api-version=2025-07-01", "", "secret/azure: SecretNotFound: Secret not found (HTTP 404)"},
		{"message without a code", "missing", `404 {"error":{"message":"Secret not found"}}`, "https://vault.test/secrets/missing?api-version=2025-07-01", "", "secret/azure: Secret not found (HTTP 404)"},
		{"code without a message", "x", `500 {"error":{"code":"Oops"}}`, "https://vault.test/secrets/x?api-version=2025-07-01", "", "secret/azure: key vault returned HTTP 500"},
		{"opaque failure", "x", `500 <html>`, "https://vault.test/secrets/x?api-version=2025-07-01", "", "secret/azure: key vault returned HTTP 500"},
		{"200 that is not json", "x", `200 <html>`, "https://vault.test/secrets/x?api-version=2025-07-01", "", "secret/azure: decode secret response: invalid character '<' looking for beginning of value"},
		{"empty value", "x", `200 {"value":""}`, "https://vault.test/secrets/x?api-version=2025-07-01", "", `secret/azure: secret "x" has empty value`},
		{"no value", "x", `200 {}`, "https://vault.test/secrets/x?api-version=2025-07-01", "", `secret/azure: secret "x" has empty value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAzure{token: tokenOK, vault: tc.vault}
			got, err := New(f.config()).Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: tc.path})
			if string(got) != tc.want || (tc.wantErr == "") != (err == nil) || (err != nil && err.Error() != tc.wantErr) {
				t.Fatalf("Resolve = %q, %v; want %q, %q", got, err, tc.want, tc.wantErr)
			}
			if len(f.reqs) != 2 {
				t.Fatalf("%d requests, want a token request then a vault request", len(f.reqs))
			}
			wantForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {"client"}, "client_secret": {"secret"}, "scope": {"https://vault.azure.net/.default"}}
			if d := cmp.Diff(wantForm, f.forms[0]); d != "" || f.reqs[0].URL.String() != "https://login.test/token" {
				t.Errorf("token request to %s (-want +got):\n%s", f.reqs[0].URL, d)
			}
			v := f.reqs[1]
			if v.Method != http.MethodGet || v.URL.String() != tc.wantURL || v.Header.Get("Authorization") != "Bearer access-tok" {
				t.Errorf("vault request = %s %s auth %q, want GET %s", v.Method, v.URL, v.Header.Get("Authorization"), tc.wantURL)
			}
		})
	}
}

func TestResolver_ReusesTheToken(t *testing.T) {
	f := &fakeAzure{token: tokenOK, vault: `200 {"value":"v"}`}
	res := New(f.config())
	for range 3 {
		if got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "s"}); err != nil || string(got) != "v" {
			t.Fatalf("Resolve = %q, %v", got, err)
		}
	}
	if len(f.forms) != 1 || len(f.reqs) != 4 {
		t.Errorf("%d token requests in %d requests, want 1 in 4", len(f.forms), len(f.reqs))
	}
}

func TestResolver_ManagedIdentity(t *testing.T) {
	f := &fakeAzure{token: `200 {"access_token":"mi-tok","expires_in":3600}`, vault: `200 {"value":"mi-secret"}`}
	got, err := New(Config{VaultURL: "https://vault.test", UseManagedIdentity: true, HTTPClient: &http.Client{Transport: f}}).
		Resolve(context.Background(), secret.Ref{Kind: secret.KindAzure, Path: "key"})
	if err != nil || string(got) != "mi-secret" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	imds, vault := f.reqs[0], f.reqs[1]
	if imds.URL.String() != "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2019-08-01&resource=https%3A%2F%2Fvault.azure.net" || imds.Header.Get("Metadata") != "true" {
		t.Errorf("metadata request = %s %v", imds.URL, imds.Header)
	}
	if vault.Header.Get("Authorization") != "Bearer mi-tok" {
		t.Errorf("vault Authorization = %q", vault.Header.Get("Authorization"))
	}
}

func TestResolver_RefusesBeforeTheVault(t *testing.T) {
	azure := func(path string) secret.Ref { return secret.Ref{Kind: secret.KindAzure, Path: path} }
	for _, tc := range []struct {
		name     string
		ref      secret.Ref
		cfg      func(*Config)
		token    string
		want     string
		wantReqs int
	}{
		{"wrong kind", secret.Ref{Kind: secret.KindEnv, Path: "x"}, nil, tokenOK, `secret/azure: wrong kind "env"`, 0},
		{"empty path", azure(" "), nil, tokenOK, "secret/azure: path is empty", 0},
		{"no name", azure("/onlyver"), nil, tokenOK, `secret/azure: empty secret name in path "/onlyver"`, 0},
		{"no version", azure("name/"), nil, tokenOK, `secret/azure: empty version in path "name/"`, 0},
		{"token refused", azure("x"), nil, `401 {"error":"invalid_client","error_description":"bad creds"}`, `secret/azure: token endpoint returned "invalid_client: bad creds" (HTTP 401)`, 1},
		{"no vault url", azure("x"), func(c *Config) { c.VaultURL = " / " }, tokenOK, "secret/azure: vault URL is not configured", 1},
		{"vault url does not parse", azure("x"), func(c *Config) { c.VaultURL = "https://bad host" }, tokenOK, `secret/azure: parse secret url: parse "https://bad host/secrets/x": invalid character " " in host name`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAzure{token: tc.token, vault: `200 {"value":"must not be read"}`}
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
	set := func(vault, managed, tenant, id, sec string) {
		t.Setenv("AZURE_KEYVAULT_URL", vault)
		t.Setenv("AZURE_USE_MANAGED_IDENTITY", managed)
		t.Setenv("AZURE_TENANT_ID", tenant)
		t.Setenv("AZURE_CLIENT_ID", id)
		t.Setenv("AZURE_CLIENT_SECRET", sec)
	}
	const vault = "https://myvault.vault.azure.net"
	for _, tc := range []struct {
		name string
		env  [5]string
		want Config
		err  string
	}{
		{"client credentials, trimmed", [5]string{" " + vault + "\n", "", " tenant-id ", " client-id ", " client-secret "}, Config{VaultURL: vault, TenantID: "tenant-id", ClientID: "client-id", ClientSecret: "client-secret"}, ""},
		{"managed identity needs no credentials", [5]string{vault, "1", "", "", ""}, Config{VaultURL: vault, UseManagedIdentity: true}, ""},
		{"managed identity drops credentials", [5]string{vault, "1", "tenant-id", "client-id", "client-secret"}, Config{VaultURL: vault, UseManagedIdentity: true}, ""},
		{"only 1 turns managed identity on", [5]string{vault, "true", "", "", ""}, Config{}, "secret/azure: AZURE_TENANT_ID is required"},
		{"no vault", [5]string{" ", "1", "tenant-id", "client-id", "client-secret"}, Config{}, "secret/azure: AZURE_KEYVAULT_URL is unset or empty"},
		{"no tenant", [5]string{vault, "", " ", "client-id", "client-secret"}, Config{}, "secret/azure: AZURE_TENANT_ID is required"},
		{"no client id", [5]string{vault, "", "tenant-id", "", "client-secret"}, Config{}, "secret/azure: AZURE_CLIENT_ID and AZURE_CLIENT_SECRET are required"},
		{"no client secret", [5]string{vault, "", "tenant-id", "client-id", " "}, Config{}, "secret/azure: AZURE_CLIENT_ID and AZURE_CLIENT_SECRET are required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set(tc.env[0], tc.env[1], tc.env[2], tc.env[3], tc.env[4])
			cfg, err := ConfigFromEnv()
			if d := cmp.Diff(tc.want, cfg); d != "" || (tc.err == "") != (err == nil) || (err != nil && err.Error() != tc.err) {
				t.Errorf("ConfigFromEnv: %v, want %q (-want +got):\n%s", err, tc.err, d)
			}
		})
	}
}
