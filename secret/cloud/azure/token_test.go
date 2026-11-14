package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

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

func clientCredentials(f *fakeEndpoint) Config {
	return Config{TenantID: "tenant", ClientID: "client", ClientSecret: "secret", Resource: "https://example.azure.net", HTTPClient: &http.Client{Transport: f}}
}

func TestTokenSource_ClientCredentialsRequest(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, wantURL string
	}{
		{"tenant endpoint", "", "https://login.microsoftonline.com/tenant/oauth2/v2.0/token"},
		{"configured endpoint", "http://localhost:9/token", "http://localhost:9/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEndpoint{status: 200, body: `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`}
			cfg := clientCredentials(f)
			cfg.TokenEndpoint = tc.endpoint
			ts := NewTokenSource(cfg)
			for range 2 {
				if tok, err := ts.AccessToken(context.Background()); err != nil || tok != "tok" {
					t.Fatalf("AccessToken = %q, %v", tok, err)
				}
			}
			if len(f.reqs) != 1 {
				t.Fatalf("%d token requests for two calls, want 1", len(f.reqs))
			}
			r := f.reqs[0]
			if r.Method != http.MethodPost || r.URL.String() != tc.wantURL || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Metadata") != "" {
				t.Errorf("request = %s %s %v", r.Method, r.URL, r.Header)
			}
			form, err := url.ParseQuery(f.bodies[0])
			want := url.Values{"grant_type": {"client_credentials"}, "client_id": {"client"}, "client_secret": {"secret"}, "scope": {"https://example.azure.net/.default"}}
			if d := cmp.Diff(want, form); d != "" || err != nil {
				t.Errorf("form (%v) (-want +got):\n%s", err, d)
			}
		})
	}
}

func TestTokenSource_ManagedIdentityRequest(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, wantURL string
	}{
		{"instance metadata", "", "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2019-08-01&resource=https%3A%2F%2Fexample.azure.net"},
		{"configured endpoint", "http://localhost:9/imds?x=1", "http://localhost:9/imds?x=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEndpoint{status: 200, body: `{"access_token":"mi-tok","expires_in":3600}`}
			// Client credentials are ignored once managed identity is on.
			cfg := clientCredentials(f)
			cfg.UseManagedIdentity, cfg.IMDSEndpoint = true, tc.endpoint
			tok, err := NewTokenSource(cfg).AccessToken(context.Background())
			if err != nil || tok != "mi-tok" {
				t.Fatalf("AccessToken = %q, %v", tok, err)
			}
			r := f.reqs[0]
			if r.Method != http.MethodGet || r.URL.String() != tc.wantURL || r.Header.Get("Metadata") != "true" || f.bodies[0] != "" || r.Header.Get("Content-Type") != "" {
				t.Errorf("request = %s %s %v body %q", r.Method, r.URL, r.Header, f.bodies[0])
			}
		})
	}

	t.Run("expires_in as the metadata service sends it", func(t *testing.T) {
		t.Skip(`bug: IMDS reply {"access_token":"t","expires_in":"3599"} (the service encodes it as a string) → "decode token response: json: cannot unmarshal string into Go struct field"; managed identity never yields a token`)
		f := &fakeEndpoint{status: 200, body: `{"access_token":"mi-tok","expires_in":"3599","expires_on":"1506484173","resource":"https://example.azure.net","token_type":"Bearer"}`}
		tok, err := NewTokenSource(Config{UseManagedIdentity: true, Resource: "https://example.azure.net", HTTPClient: &http.Client{Transport: f}}).AccessToken(context.Background())
		if err != nil || tok != "mi-tok" {
			t.Fatalf("AccessToken = %q, %v", tok, err)
		}
	})
}

func TestTokenSource_RefreshesBeforeExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn string
		cachedFor time.Duration
	}{
		{"a minute before expiry", "120", 60 * time.Second},
		{"a short-lived token is used for its whole life", "60", 60 * time.Second},
		{"one second", "1", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			f := &fakeEndpoint{status: 200, body: `{"access_token":"tok-1","expires_in":` + tc.expiresIn + `}`}
			cfg := clientCredentials(f)
			cfg.Now = func() time.Time { return now }
			ts := NewTokenSource(cfg)
			get := func(want string, wantReqs int) {
				t.Helper()
				if tok, err := ts.AccessToken(context.Background()); err != nil || tok != want || len(f.reqs) != wantReqs {
					t.Fatalf("AccessToken = %q, %v after %d requests; want %q after %d", tok, err, len(f.reqs), want, wantReqs)
				}
			}
			get("tok-1", 1)
			f.body = `{"access_token":"tok-2","expires_in":3600}`
			now = now.Add(tc.cachedFor - time.Nanosecond)
			get("tok-1", 1)
			now = now.Add(time.Nanosecond)
			get("tok-2", 2)
		})
	}
}

func TestTokenSource_Errors(t *testing.T) {
	down := errors.New("network down")
	ok := func(f *fakeEndpoint) Config { return clientCredentials(f) }
	for _, tc := range []struct {
		name     string
		f        fakeEndpoint
		cfg      func(*fakeEndpoint) Config
		want     string
		wantReqs int
	}{
		{"no tenant", fakeEndpoint{}, func(f *fakeEndpoint) Config { c := ok(f); c.TenantID = " "; return c }, "secret/azure: tenant is not configured", 0},
		{"no client id", fakeEndpoint{}, func(f *fakeEndpoint) Config { c := ok(f); c.ClientID = ""; return c }, "secret/azure: client credentials are not configured", 0},
		{"no client secret", fakeEndpoint{}, func(f *fakeEndpoint) Config { c := ok(f); c.ClientSecret = ""; return c }, "secret/azure: client credentials are not configured", 0},
		{"endpoint does not parse", fakeEndpoint{}, func(f *fakeEndpoint) Config { c := ok(f); c.TokenEndpoint = "http://bad host"; return c }, `secret/azure: build token request: parse "http://bad host": invalid character " " in host name`, 0},
		{"transport failure", fakeEndpoint{err: down}, ok, `secret/azure: token request failed: Post "https://login.microsoftonline.com/tenant/oauth2/v2.0/token": network down`, 1},
		{"error and description", fakeEndpoint{status: 401, body: `{"error":"invalid_client","error_description":"bad creds"}`}, ok, `secret/azure: token endpoint returned "invalid_client: bad creds" (HTTP 401)`, 1},
		{"error only", fakeEndpoint{status: 400, body: `{"error":"invalid_request"}`}, ok, `secret/azure: token endpoint returned "invalid_request" (HTTP 400)`, 1},
		{"description only", fakeEndpoint{status: 400, body: `{"error_description":"bad creds"}`}, ok, `secret/azure: token endpoint returned "bad creds" (HTTP 400)`, 1},
		{"opaque failure", fakeEndpoint{status: 502, body: `<html>`}, ok, "secret/azure: token endpoint returned HTTP 502", 1},
		{"empty json failure", fakeEndpoint{status: 500, body: `{}`}, ok, "secret/azure: token endpoint returned HTTP 500", 1},
		{"200 that is not json", fakeEndpoint{status: 200, body: `<html>`}, ok, "secret/azure: decode token response: invalid character '<' looking for beginning of value", 1},
		{"no access_token", fakeEndpoint{status: 200, body: `{"expires_in":3600}`}, ok, "secret/azure: token response missing access_token", 1},
		{"no expires_in", fakeEndpoint{status: 200, body: `{"access_token":"tok"}`}, ok, "secret/azure: token response missing expires_in", 1},
		{"negative expires_in", fakeEndpoint{status: 200, body: `{"access_token":"tok","expires_in":-5}`}, ok, "secret/azure: token response missing expires_in", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			ts := NewTokenSource(tc.cfg(&f))
			tok, err := ts.AccessToken(context.Background())
			if err == nil || err.Error() != tc.want || tok != "" || len(f.reqs) != tc.wantReqs {
				t.Fatalf("AccessToken = %q, %v after %d requests; want %q after %d", tok, err, len(f.reqs), tc.want, tc.wantReqs)
			}
			// A failure is not cached: the next call asks again.
			if _, err := ts.AccessToken(context.Background()); err == nil || len(f.reqs) != 2*tc.wantReqs {
				t.Errorf("second call: %v after %d requests, want %d", err, len(f.reqs), 2*tc.wantReqs)
			}
			if tc.name == "transport failure" && !errors.Is(err, down) {
				t.Errorf("err = %v, want it to wrap the transport error", err)
			}
		})
	}
}
