package oauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/wyolet/relay/sdk/oauth"
)

func testConfig(srv *httptest.Server) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    "client-abc",
		RedirectURL: "https://app.example/callback",
		Scopes:      []string{"user:inference"},
		Endpoint: oauth2.Endpoint{
			AuthURL:       srv.URL + "/authorize",
			TokenURL:      srv.URL + "/token",
			DeviceAuthURL: srv.URL + "/device",
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

func TestAuthorizeURL_CarriesPKCEChallengeAndState(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()

	f := oauth.New(testConfig(srv))
	raw, verifier := f.AuthorizeURL("state-xyz")
	if verifier == "" {
		t.Fatal("empty PKCE verifier")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if got := q.Get("state"); got != "state-xyz" {
		t.Errorf("state: want state-xyz, got %q", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method: want S256, got %q", got)
	}
	if q.Get("code_challenge") == "" {
		t.Error("missing code_challenge")
	}
	if got := q.Get("client_id"); got != "client-abc" {
		t.Errorf("client_id: want client-abc, got %q", got)
	}
}

func TestExchange_SendsVerifierAndParsesToken(t *testing.T) {
	var gotVerifier, gotCode, gotGrant string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotCode = r.Form.Get("code")
		gotVerifier = r.Form.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-1","token_type":"bearer","refresh_token":"rt-1","expires_in":3600}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := oauth.New(testConfig(srv))
	_, verifier := f.AuthorizeURL("s")
	tok, err := f.Exchange(context.Background(), "auth-code-9", verifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotGrant != "authorization_code" {
		t.Errorf("grant_type: want authorization_code, got %q", gotGrant)
	}
	if gotCode != "auth-code-9" {
		t.Errorf("code: want auth-code-9, got %q", gotCode)
	}
	if gotVerifier != verifier {
		t.Errorf("code_verifier: want %q, got %q", verifier, gotVerifier)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Errorf("token: got access=%q refresh=%q", tok.AccessToken, tok.RefreshToken)
	}
}

func TestRefresh_ValidTokenNoNetwork(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint must not be hit for a valid token")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := oauth.New(testConfig(srv))
	in := &oauth.Token{AccessToken: "still-good", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}
	out, err := f.Refresh(context.Background(), in)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.AccessToken != "still-good" {
		t.Errorf("want unchanged token, got %q", out.AccessToken)
	}
}

func TestRefresh_ExpiredTokenExchangesRefreshToken(t *testing.T) {
	var gotGrant, gotRefresh string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotRefresh = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-2","token_type":"bearer","refresh_token":"rt-2","expires_in":3600}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := oauth.New(testConfig(srv))
	in := &oauth.Token{AccessToken: "expired", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute)}
	out, err := f.Refresh(context.Background(), in)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotGrant != "refresh_token" {
		t.Errorf("grant_type: want refresh_token, got %q", gotGrant)
	}
	if gotRefresh != "rt-1" {
		t.Errorf("refresh_token sent: want rt-1, got %q", gotRefresh)
	}
	if out.AccessToken != "at-2" || out.RefreshToken != "rt-2" {
		t.Errorf("rotated token: got access=%q refresh=%q", out.AccessToken, out.RefreshToken)
	}
}

func TestDeviceFlow_AuthThenToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"WXYZ-1234","verification_uri":"https://ex/dev","interval":1,"expires_in":300}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if !strings.Contains(r.Form.Get("grant_type"), "device_code") {
			t.Errorf("device grant_type: got %q", r.Form.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-dev","token_type":"bearer","refresh_token":"rt-dev","expires_in":3600}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := oauth.New(testConfig(srv))
	da, err := f.DeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}
	if da.UserCode != "WXYZ-1234" {
		t.Errorf("user_code: got %q", da.UserCode)
	}
	tok, err := f.DeviceToken(context.Background(), da)
	if err != nil {
		t.Fatalf("DeviceToken: %v", err)
	}
	if tok.AccessToken != "at-dev" {
		t.Errorf("device token: got %q", tok.AccessToken)
	}
}
