package client

import (
	"testing"
	"time"
)

// Audit 2026-07-04 (audit-sdk-core.md, P2): RelayWS targets the relay but
// calls New directly, bypassing the WR_* env fallback and missing-config
// validation that Relay() applies. client.go documents "Only the relay
// target consults them" for WR_BASE_URL / WR_API_KEY / WR_USAGE /
// WR_HEADERS / WR_TIMEOUT — RelayWS is a relay target, so an app switching
// Relay→RelayWS (the advertised upgrade for agent loops) silently loses its
// env-provided key, usage echo, headers, and timeout, and gets an opaque
// "ws dial" error instead of Relay's crisp missing-config error.
//
// Construction/config level only — no websocket connection is made.
func TestRelayWSHonorsRelayEnvConfig(t *testing.T) {
	t.Skip("audit 2026-07-04: RelayWS bypasses WR_* env fallback + config validation — known-broken, unskip with the fix")
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv(EnvBaseURL, "http://relay-env.example:8080")
		t.Setenv(EnvAPIKey, "rk-env-key")
		t.Setenv(EnvUsage, "full")
		t.Setenv(EnvHeaders, "X-Env-Header=hello")
		t.Setenv(EnvTimeout, "42s")

		c := RelayWS("", "")

		if got, want := c.baseURL, "http://relay-env.example:8080"; got != want {
			t.Errorf("baseURL: RelayWS ignored %s: got %q, want %q", EnvBaseURL, got, want)
		}
		if got, want := c.apiKey, "rk-env-key"; got != want {
			t.Errorf("apiKey: RelayWS ignored %s: got %q, want %q", EnvAPIKey, got, want)
		}
		if got, want := c.headers[headerUsage], "full"; got != want {
			t.Errorf("%s header: RelayWS ignored %s: got %q, want %q", headerUsage, EnvUsage, got, want)
		}
		if got, want := c.headers["X-Env-Header"], "hello"; got != want {
			t.Errorf("X-Env-Header: RelayWS ignored %s: got %q, want %q", EnvHeaders, got, want)
		}
		if got, want := c.syncTimeout, 42*time.Second; got != want {
			t.Errorf("syncTimeout: RelayWS ignored %s: got %v, want %v", EnvTimeout, got, want)
		}
	})

	t.Run("missing config error", func(t *testing.T) {
		t.Setenv(EnvBaseURL, "")
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvUsage, "")
		t.Setenv(EnvHeaders, "")
		t.Setenv(EnvTimeout, "")

		c := RelayWS("", "")

		if c.configErr == nil {
			t.Errorf("configErr: RelayWS(%q, %q) with no WR_* env set no deferred construction error; Relay() reports %q",
				"", "", Relay("", "").configErr)
		}
	})
}
