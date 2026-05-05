package httpheader

import (
	"net/http"
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
