package httpheader

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestStrip_RemovesAuthorization(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer foo"}, "Content-Type": {"application/json"}}
	Strip(h)
	if h.Get("Authorization") != "" {
		t.Error("Authorization should be stripped (captured into ctx upstream of Strip)")
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should be preserved")
	}
}

func TestStrip_RemovesHopByHop(t *testing.T) {
	h := http.Header{
		"Proxy-Authorization": {"Basic bar"},
		"Keep-Alive":          {"timeout=5"},
		"Te":                  {"trailers"},
	}
	Strip(h)
	for _, name := range []string{"Proxy-Authorization", "Keep-Alive", "Te"} {
		if h.Get(name) != "" {
			t.Errorf("%s should be stripped (hop-by-hop)", name)
		}
	}
}

func TestStrip_RemovesXWRHeaders(t *testing.T) {
	h := http.Header{
		"X-Wr-Api-Key":       {"wr_foo"},
		"X-Wr-Proxy-Mode":    {"Proxy"},
		"X-Wr-Upstream-Host": {"anthropic"},
	}
	Strip(h)
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-wr-") {
			t.Errorf("%s should be stripped (X-WR-* denylist)", k)
		}
	}
}

func TestStrip_RemovesCookie(t *testing.T) {
	h := http.Header{"Cookie": {"sid=abc"}, "Content-Type": {"application/json"}}
	Strip(h)
	if h.Get("Cookie") != "" {
		t.Error("Cookie should be stripped")
	}
}

func TestStrip_PreservesUnknownHeaders(t *testing.T) {
	h := http.Header{
		"Content-Type":             {"application/json"},
		"Accept":                   {"*/*"},
		"User-Agent":               {"relay-test"},
		"Anthropic-Beta":           {"prompt-caching-2024-07-31"},
		"X-Stainless-Lang":         {"js"},
		"X-Claude-Code-Session-Id": {"sess_123"},
		"X-Some-Vendor-Header":     {"foo"},
	}
	Strip(h)
	for _, name := range []string{
		"Content-Type", "Accept", "User-Agent",
		"Anthropic-Beta", "X-Stainless-Lang", "X-Claude-Code-Session-Id",
		"X-Some-Vendor-Header",
	} {
		if h.Get(name) == "" {
			t.Errorf("%s should be preserved (negative-strip policy)", name)
		}
	}
}

func TestStrip_HonorsConnectionList(t *testing.T) {
	h := http.Header{
		"Connection": {"x-custom, X-Other"},
		"X-Custom":   {"foo"},
		"X-Other":    {"bar"},
	}
	Strip(h)
	if h.Get("X-Custom") != "" {
		t.Error("X-Custom should be stripped (listed in Connection)")
	}
	if h.Get("X-Other") != "" {
		t.Error("X-Other should be stripped (listed in Connection)")
	}
}

func TestSanitizeUpstreamResponse_RemovesHopByHop(t *testing.T) {
	h := http.Header{
		"Connection":          {"keep-alive"},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Authenticate":  {"Basic"},
		"Proxy-Authorization": {"Basic foo"},
		"Te":                  {"trailers"},
		"Trailers":            {"X-Foo"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"websocket"},
		"Content-Type":        {"application/json"},
	}
	SanitizeUpstreamResponse(h)
	for _, name := range HopByHop {
		if h.Get(name) != "" {
			t.Errorf("%s should be stripped", name)
		}
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should be preserved")
	}
}

func TestMatch_CaseInsensitive(t *testing.T) {
	if !Match("authorization", []string{"Authorization"}) {
		t.Error("expected match")
	}
	if !Match("AUTHORIZATION", []string{"authorization"}) {
		t.Error("expected case-insensitive match")
	}
}

func TestMatch_PrefixWildcard(t *testing.T) {
	if !Match("x-wr-api-key", []string{"X-WR-*"}) {
		t.Error("expected prefix match")
	}
	if Match("x-other", []string{"X-WR-*"}) {
		t.Error("x-other should not match X-WR-*")
	}
}

func TestSafeUpstreamError_RedactsIP(t *testing.T) {
	err := errors.New("dial tcp 192.168.0.109:443: connect: connection refused")
	msg := SafeUpstreamError("openai", err)
	if strings.Contains(msg, "192.168.0.109") {
		t.Errorf("IP not redacted: %q", msg)
	}
	if !strings.Contains(msg, "openai") {
		t.Errorf("provider name missing: %q", msg)
	}
}

func TestSafeUpstreamError_RedactsURL(t *testing.T) {
	err := errors.New("Post \"https://api.openai.com/v1/foo\": dial tcp: connection refused")
	msg := SafeUpstreamError("openai", err)
	if strings.Contains(msg, "https://") {
		t.Errorf("URL not redacted: %q", msg)
	}
	if !strings.Contains(msg, "openai") {
		t.Errorf("provider name missing: %q", msg)
	}
}
