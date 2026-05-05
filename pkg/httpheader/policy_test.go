package httpheader

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestStripInbound_RemovesAuth(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer foo"}, "Content-Type": {"application/json"}}
	StripInbound(h)
	if h.Get("Authorization") != "" {
		t.Error("Authorization not stripped")
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should be preserved")
	}
}

func TestStripInbound_RemovesProxyAuth(t *testing.T) {
	h := http.Header{"Proxy-Authorization": {"Basic bar"}}
	StripInbound(h)
	if h.Get("Proxy-Authorization") != "" {
		t.Error("Proxy-Authorization not stripped")
	}
}

func TestStripInbound_RemovesXOpenAIPrefix(t *testing.T) {
	h := http.Header{"X-Openai-Organization": {"org-123"}, "Accept": {"*/*"}}
	StripInbound(h)
	if h.Get("X-Openai-Organization") != "" {
		t.Error("X-OpenAI-Organization not stripped")
	}
	if h.Get("Accept") == "" {
		t.Error("Accept should be preserved")
	}
}

func TestStripInbound_PreservesNormalHeaders(t *testing.T) {
	h := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"*/*"},
		"User-Agent":   {"relay-test"},
	}
	StripInbound(h)
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should be preserved")
	}
	if h.Get("Accept") == "" {
		t.Error("Accept should be preserved")
	}
	if h.Get("User-Agent") == "" {
		t.Error("User-Agent should be preserved")
	}
}

func TestStripInbound_HonorsConnectionList(t *testing.T) {
	h := http.Header{
		"Connection": {"x-custom, X-Other"},
		"X-Custom":   {"foo"},
		"X-Other":    {"bar"},
		"Keep":       {"alive"},
	}
	StripInbound(h)
	if h.Get("X-Custom") != "" {
		t.Error("X-Custom should be stripped (listed in Connection)")
	}
	if h.Get("X-Other") != "" {
		t.Error("X-Other should be stripped (listed in Connection)")
	}
}

func TestSanitizeUpstreamResponse_RemovesHopByHop(t *testing.T) {
	h := http.Header{
		"Connection":        {"keep-alive"},
		"Keep-Alive":        {"timeout=5"},
		"Proxy-Authenticate": {"Basic"},
		"Proxy-Authorization": {"Basic foo"},
		"Te":                {"trailers"},
		"Trailers":          {"X-Foo"},
		"Transfer-Encoding": {"chunked"},
		"Upgrade":           {"websocket"},
		"Content-Type":      {"application/json"},
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
	if !Match("x-openai-org", []string{"X-OpenAI-*"}) {
		t.Error("expected prefix match")
	}
	if Match("x-other", []string{"X-OpenAI-*"}) {
		t.Error("x-other should not match X-OpenAI-*")
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

func TestStripInbound_RemovesXRelayMetadata(t *testing.T) {
	h := http.Header{
		"X-Relay-Metadata": {"env=prod,team=backend"},
		"Content-Type":     {"application/json"},
	}
	StripInbound(h)
	if h.Get("X-Relay-Metadata") != "" {
		t.Error("X-Relay-Metadata should be stripped from inbound headers")
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type should be preserved")
	}
}
