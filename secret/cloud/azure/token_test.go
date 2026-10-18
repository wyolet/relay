package azure

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenSource_ClientCredentialsScopeFromResource(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil ||
			r.Form.Get("grant_type") != "client_credentials" ||
			r.Form.Get("client_id") != "client" ||
			r.Form.Get("client_secret") != "secret" ||
			r.Form.Get("scope") != "https://example.azure.net/.default" {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	}))
	defer srv.Close()

	ts := NewTokenSource(Config{
		TenantID:      "tenant",
		ClientID:      "client",
		ClientSecret:  "secret",
		Resource:      "https://example.azure.net",
		TokenEndpoint: srv.URL,
		HTTPClient:    http.DefaultClient,
	})
	for i := 0; i < 2; i++ {
		tok, err := ts.AccessToken(context.Background())
		if err != nil || tok != "tok" {
			t.Fatalf("token %d: %q err %v", i, tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("token calls: got %d want 1", calls.Load())
	}
}

func TestTokenSource_ManagedIdentityDefaultURL(t *testing.T) {
	var seen *http.Request
	ts := NewTokenSource(Config{
		UseManagedIdentity: true,
		Resource:           "https://example.azure.net",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"mi-tok","expires_in":3600}`)),
				Header:     http.Header{},
			}, nil
		})},
	})
	tok, err := ts.AccessToken(context.Background())
	if err != nil || tok != "mi-tok" {
		t.Fatalf("token: %q err %v", tok, err)
	}
	if seen.Method != http.MethodGet || seen.Header.Get("Metadata") != "true" {
		t.Fatalf("request: %s %v", seen.Method, seen.Header)
	}
	if got := seen.URL.String(); got != "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2019-08-01&resource=https%3A%2F%2Fexample.azure.net" {
		t.Fatalf("imds url: %s", got)
	}
}

func TestTokenSource_MissingClientCredentials(t *testing.T) {
	_, err := NewTokenSource(Config{TenantID: "tenant"}).AccessToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "client credentials are not configured") {
		t.Fatalf("got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
