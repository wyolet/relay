package bitwarden

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginSendsDeviceName(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		seen = r.Form.Get("deviceName")
		_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: "tok", ExpiresIn: 3600})
	}))
	defer srv.Close()

	for _, tc := range []struct{ configured, want string }{
		{"", "wyolet-secret"},
		{"my-service", "my-service"},
	} {
		c := newClient(Config{BaseURL: srv.URL, ClientID: "id", ClientSecret: "secret", DeviceName: tc.configured})
		if _, err := c.loginWithAPIKey(context.Background()); err != nil {
			t.Fatal(err)
		}
		if seen != tc.want {
			t.Fatalf("DeviceName %q: server saw %q, want %q", tc.configured, seen, tc.want)
		}
	}
}
