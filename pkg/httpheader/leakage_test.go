package httpheader_test

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/internal/provider/openai"
	"github.com/wyolet/relay/pkg/transport"
)

// sensitiveHeaders returns a battery of ≥10 sensitive header names mapped to
// unique random sentinel values. Any of these appearing on an upstream request
// would indicate a leakage bug.
func sensitiveHeaders() map[string]string {
	suffix := fmt.Sprintf("%08x", rand.Uint32())
	return map[string]string{
		"Cookie":            "session=sentinel-" + suffix + "-cookie",
		"X-Forwarded-For":  "203.0.113.1 sentinel-" + suffix + "-xff",
		"X-Real-IP":        "203.0.113.2 sentinel-" + suffix + "-xrip",
		"X-Custom-1":       "sentinel-" + suffix + "-custom1",
		"X-Custom-2":       "sentinel-" + suffix + "-custom2",
		"X-Frame-Options":  "DENY sentinel-" + suffix + "-xfo",
		"Forwarded":        "for=203.0.113.3 sentinel-" + suffix + "-fwd",
		"Via":              "1.1 proxy sentinel-" + suffix + "-via",
		"Origin":           "https://evil.example sentinel-" + suffix + "-origin",
		"Referer":          "https://evil.example/page sentinel-" + suffix + "-referer",
		"X-Sentinel-Alpha": "sentinel-" + suffix + "-alpha",
		"X-Sentinel-Beta":  "sentinel-" + suffix + "-beta",
	}
}

// TestStripInbound_NoLeakage verifies that StripInbound removes all non-allowlisted
// sensitive headers and their sentinel values cannot be found in the surviving map.
func TestStripInbound_NoLeakage(t *testing.T) {
	sensitive := sensitiveHeaders()

	h := http.Header{
		"Content-Type": {"application/json"},
		"User-Agent":   {"relay-test/1"},
		"Accept":       {"*/*"},
	}
	for name, val := range sensitive {
		h.Set(name, val)
	}

	httpheader.StripInbound(h)

	// No sentinel value should survive in any header.
	for sentinelName, sentinelVal := range sensitive {
		for hdrName, vals := range h {
			for _, v := range vals {
				if strings.Contains(v, sentinelVal) {
					t.Errorf("sentinel from inbound header %q leaked into header %q=%q after StripInbound",
						sentinelName, hdrName, v)
				}
			}
		}
		// Direct lookup by original name must also be empty.
		if got := h.Get(sentinelName); got != "" {
			t.Errorf("header %q still present after StripInbound: %q", sentinelName, got)
		}
	}

	// Allowlisted headers must survive.
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should survive StripInbound")
	}
	if h.Get("User-Agent") == "" {
		t.Error("User-Agent should survive StripInbound")
	}
}

// TestOpenAIClient_NoLeakage spawns a capturing httptest.Server and fires a request
// through the OpenAI provider client. It verifies that:
//   - No sentinel value from the simulated inbound headers appears on the upstream request.
//   - Authorization: Bearer <upstream-key> IS present (provider injection works).
//   - X-Relay-Metadata is NOT present (M4 contract).
//   - Only allowlisted headers appear on the upstream request.
func TestOpenAIClient_NoLeakage(t *testing.T) {
	sensitive := sensitiveHeaders()

	var capturedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Simulate what the API layer does: strip inbound, then forward only body.
	// We verify that even if an attacker sends all sensitive headers, StripInbound
	// removes them before they can be stored or forwarded.
	inbound := http.Header{
		"Content-Type":     {"application/json"},
		"X-Relay-Metadata": {"env=prod,team=backend"},
	}
	for name, val := range sensitive {
		inbound.Set(name, val)
	}
	httpheader.StripInbound(inbound)

	// The OpenAI client builds a fresh outbound request — it never receives
	// inbound headers. This call exercises the actual upstream HTTP path.
	c := openai.New(srv.URL)
	out := make(chan *transport.Message, 64)
	ctx := context.Background()
	go func() {
		c.ChatCompletions(ctx, []byte(`{"model":"gpt-4","messages":[]}`), "sk-upstream-key", out)
	}()
	for range out {
	}

	if capturedHeaders == nil {
		t.Fatal("upstream server never received a request")
	}

	// Assert 1: no sentinel value appears in any upstream header.
	for sentinelName, sentinelVal := range sensitive {
		for hdrName, vals := range capturedHeaders {
			for _, v := range vals {
				if strings.Contains(v, sentinelVal) {
					t.Errorf("sentinel from inbound header %q leaked to upstream header %q=%q",
						sentinelName, hdrName, v)
				}
			}
		}
	}

	// Assert 2: provider-injected Authorization is present with the upstream key.
	auth := capturedHeaders.Get("Authorization")
	if auth != "Bearer sk-upstream-key" {
		t.Errorf("Authorization = %q; want %q", auth, "Bearer sk-upstream-key")
	}

	// Assert 3: X-Relay-Metadata is NOT present on upstream (M4 contract).
	if v := capturedHeaders.Get("X-Relay-Metadata"); v != "" {
		t.Errorf("X-Relay-Metadata leaked to upstream: %q", v)
	}

	// Assert 4: only allowlisted headers (or Go's default transport additions) are present.
	outboundPermitted := map[string]bool{
		"content-type":    true,
		"content-length":  true,
		"accept":          true,
		"accept-encoding": true,
		"user-agent":      true,
		"x-request-id":    true,
		"openai-beta":     true,
		"authorization":   true, // injected by provider client
	}
	for name := range capturedHeaders {
		lower := strings.ToLower(name)
		if !outboundPermitted[lower] {
			t.Errorf("unexpected header present on upstream request: %q = %q", name, capturedHeaders.Get(name))
		}
	}
}
