package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/internal/config"
)

// The CLI's default target has to be the control listener's default port, or every subcommand goes to the wrong socket.
func TestDefaultControlURLMatchesTheControlPort(t *testing.T) {
	clearRelayEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://localhost:" + cfg.ControlPort; DefaultControlURL != want {
		t.Fatalf("DefaultControlURL = %q, want %q", DefaultControlURL, want)
	}
}

func TestNewControlClient(t *testing.T) {
	for _, tc := range []struct {
		name               string
		url, token         string
		envURL, envToken   string
		wantURL, wantToken string
	}{
		{"flags win over the environment", "http://flag.example/", "flag-tok", "http://env.example", "env-tok", "http://flag.example", "flag-tok"},
		{"environment fills what is not passed", "", "", "http://env.example/", "env-tok", "http://env.example", "env-tok"},
		{"default target, no token", "", "", "", "", "http://localhost:8081", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RELAY_URL", tc.envURL)
			t.Setenv("RELAY_ADMIN_TOKEN", tc.envToken)
			c := newControlClient(tc.url, tc.token)
			if c.baseURL != tc.wantURL || c.token != tc.wantToken {
				t.Errorf("client = %q %q, want %q %q", c.baseURL, c.token, tc.wantURL, tc.wantToken)
			}
		})
	}
}

func TestControlClient_Do(t *testing.T) {
	srv := newControlServer(t, http.StatusTeapot, "  short and stout\n")
	t.Setenv("RELAY_ADMIN_TOKEN", "")
	out, err := newControlClient(srv.srv.URL, "").do("GET", "/api/x?y=1", "", nil)
	var he *httpError
	if !errors.As(err, &he) || he.Status != http.StatusTeapot || err.Error() != "server returned 418: short and stout" || string(out) != "  short and stout\n" {
		t.Errorf("do = %q, %v", out, err)
	}
	// No token and no content type: neither header is sent.
	want := controlRequest{Method: "GET", Path: "/api/x", Query: "y=1"}
	if got := srv.requests(); len(got) != 1 || got[0] != want {
		t.Errorf("requests = %+v, want %+v", got, want)
	}
	if _, err := newControlClient("http://127.0.0.1:1", "").do("GET", "/", "", nil); err == nil || errors.As(err, &he) {
		t.Errorf("unreachable server: err = %v, want a transport error", err)
	}
}
