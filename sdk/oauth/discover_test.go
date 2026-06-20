package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/sdk/oauth"
)

func metadataServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

func TestDiscover_FillsEmptyEndpoints(t *testing.T) {
	srv := metadataServer(t, `{
		"authorization_endpoint": "https://idp/auth",
		"token_endpoint": "https://idp/token",
		"device_authorization_endpoint": "https://idp/device"
	}`)
	defer srv.Close()

	cfg := oauth.ProviderConfig{ClientID: "cid", Issuer: srv.URL}
	got, err := cfg.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.AuthURL != "https://idp/auth" || got.TokenURL != "https://idp/token" || got.DeviceAuthURL != "https://idp/device" {
		t.Errorf("endpoints not filled: %+v", got)
	}
	if got.ClientID != "cid" {
		t.Errorf("clientId clobbered: %q", got.ClientID)
	}
}

func TestDiscover_PreservesExplicitEndpoint(t *testing.T) {
	srv := metadataServer(t, `{"authorization_endpoint":"https://idp/auth","token_endpoint":"https://idp/token"}`)
	defer srv.Close()

	// AuthURL pre-set, TokenURL empty → discovery fills only TokenURL.
	cfg := oauth.ProviderConfig{Issuer: srv.URL, AuthURL: "https://custom/auth"}
	got, err := cfg.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.AuthURL != "https://custom/auth" {
		t.Errorf("explicit AuthURL overridden: %q", got.AuthURL)
	}
	if got.TokenURL != "https://idp/token" {
		t.Errorf("TokenURL not filled: %q", got.TokenURL)
	}
}

func TestDiscover_NoOpWhenCoreEndpointsSet(t *testing.T) {
	// Both core endpoints set + no Issuer + no server → must not error or fetch.
	cfg := oauth.ProviderConfig{AuthURL: "https://a", TokenURL: "https://t"}
	got, err := cfg.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got.AuthURL != "https://a" || got.TokenURL != "https://t" {
		t.Errorf("unexpected change: %+v", got)
	}
}

func TestDiscover_NoIssuerWhenEndpointsMissing(t *testing.T) {
	cfg := oauth.ProviderConfig{ClientID: "cid"} // nothing to go on
	if _, err := cfg.Discover(context.Background(), nil); err == nil {
		t.Fatal("expected error when endpoints empty and no issuer")
	}
}

func TestDiscover_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	cfg := oauth.ProviderConfig{Issuer: srv.URL}
	if _, err := cfg.Discover(context.Background(), nil); err == nil {
		t.Fatal("expected error on non-200 discovery response")
	}
}
