package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/config"
)

// Single-user authorization makes every authenticated caller an admin; with
// open registration that is every user the IdP knows, so boot says so.
func TestBootWarnsOnSingleUserWithOpenRegistration(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	open := &settings.AuthOIDC{Enabled: true, Registration: "open"}
	closed := &settings.AuthOIDC{Enabled: true, Registration: "closed"}
	cases := []struct {
		mode string
		oidc *settings.AuthOIDC
		warn bool
	}{
		{config.AuthzSingle, open, true},
		{config.AuthzSingle, closed, false},
		{config.AuthzSingle, &settings.AuthOIDC{Registration: "open"}, false},
		{config.AuthzRBAC, open, false},
		{config.AuthzSingle, nil, false},
	}
	for _, tc := range cases {
		buf.Reset()
		warnSingleUserOpenRegistration(tc.mode, tc.oidc)
		if got := strings.Contains(buf.String(), "level=WARN"); got != tc.warn {
			t.Errorf("mode=%s oidc=%+v: warned=%v, want %v (%s)", tc.mode, tc.oidc, got, tc.warn, buf.String())
		}
	}
}
