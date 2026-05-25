package inference

import (
	"net/http"
	"testing"
)

// forwardHeaders must strip the relay credential and all relay-internal
// control headers before the inbound headers are handed to the adapter for
// the upstream call. This is the leak A4 guards: for upstreams whose auth
// header is not Authorization (Anthropic x-api-key, Gemini x-goog-api-key),
// an un-stripped inbound Authorization / X-Api-Key / X-WR-* would otherwise
// ride along to the provider.
func TestForwardHeaders_StripsRelayCredentialsAndControlHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer relay-secret") // relay key (Bearer + Anthropic-SDK)
	in.Set("X-Api-Key", "relay-secret")            // relay key (Anthropic-SDK convention)
	in.Set("X-WR-API-Key", "relay-secret")         // relay-internal
	in.Set("X-WR-Upstream-Host", "openai")         // relay-internal
	in.Set("X-WR-Proxy-Mode", "Proxy")             // relay-internal
	in.Set("Cookie", "relay_session=abc")          // session cookie
	in.Set("X-Relay-Metadata", "trace=1")          // relay-internal
	in.Set("Content-Type", "application/json")     // benign — must pass
	in.Set("X-Custom-Caller-Header", "keep-me")    // benign — must pass

	out := forwardHeaders(in)

	for _, leaked := range []string{
		"Authorization", "X-Api-Key", "X-WR-API-Key", "X-WR-Upstream-Host",
		"X-WR-Proxy-Mode", "Cookie", "X-Relay-Metadata",
	} {
		if v := out.Get(leaked); v != "" {
			t.Errorf("header %q leaked to upstream: %q", leaked, v)
		}
	}
	if out.Get("Content-Type") != "application/json" {
		t.Error("benign Content-Type was stripped")
	}
	if out.Get("X-Custom-Caller-Header") != "keep-me" {
		t.Error("benign caller header was stripped")
	}
}

// forwardHeaders must not mutate the caller's request headers (they are read
// later for logging / post-flight).
func TestForwardHeaders_DoesNotMutateOriginal(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer relay-secret")
	in.Set("X-WR-API-Key", "relay-secret")

	_ = forwardHeaders(in)

	if in.Get("Authorization") == "" || in.Get("X-WR-API-Key") == "" {
		t.Error("forwardHeaders mutated the original request headers")
	}
}
