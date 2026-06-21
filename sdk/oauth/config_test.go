package oauth_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/oauth"
)

func TestProviderConfig_OAuth2AndAuthorizeURL(t *testing.T) {
	cfg := oauth.ProviderConfig{
		ClientID:    "cid-1",
		AuthURL:     "https://provider.example/oauth/authorize",
		TokenURL:    "https://provider.example/oauth/token",
		RedirectURI: "https://provider.example/callback",
		Scopes:      []string{"user:inference", "user:profile"},
		AuthParams:  map[string]string{"code": "true"},
	}

	oc := cfg.OAuth2()
	if oc.ClientID != "cid-1" || oc.Endpoint.TokenURL != cfg.TokenURL {
		t.Fatalf("OAuth2 mapping wrong: %+v", oc)
	}

	f := oauth.New(oc)
	raw, verifier := f.AuthorizeURL("st", cfg.AuthCodeOptions()...)
	if verifier == "" {
		t.Fatal("empty verifier")
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if !strings.HasPrefix(raw, cfg.AuthURL) {
		t.Errorf("authorize host: got %q", raw)
	}
	if q.Get("client_id") != "cid-1" {
		t.Errorf("client_id: got %q", q.Get("client_id"))
	}
	if q.Get("code") != "true" {
		t.Errorf("authParams not applied: code=%q", q.Get("code"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("pkce method: got %q", q.Get("code_challenge_method"))
	}
	if !strings.Contains(q.Get("scope"), "user:inference") {
		t.Errorf("scope: got %q", q.Get("scope"))
	}
}
