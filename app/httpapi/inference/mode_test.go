package inference

import (
	"errors"
	"net/http"
	"testing"
)

func req(headers map[string]string) *http.Request {
	r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.5:54321"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClassify_Normal_AuthorizationBearer(t *testing.T) {
	c, err := Classify(req(map[string]string{
		"Authorization": "Bearer wr_abc",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Mode != ModeNormal {
		t.Fatalf("mode: want Normal, got %v", c.Mode)
	}
	if c.RelayKey != "wr_abc" {
		t.Fatalf("relay key: want wr_abc, got %q", c.RelayKey)
	}
	if c.UpstreamAuth != "" || c.UpstreamHost != "" {
		t.Fatalf("normal mode should not carry upstream fields: %+v", c)
	}
}

func TestClassify_Normal_XWRAPIKeyPrecedence(t *testing.T) {
	c, _ := Classify(req(map[string]string{
		"X-WR-API-Key":  "wr_via_header",
		"Authorization": "Bearer wr_via_auth",
	}))
	if c.RelayKey != "wr_via_header" {
		t.Fatalf("want X-WR-API-Key to win, got %q", c.RelayKey)
	}
}

func TestClassify_ProxyAuthed(t *testing.T) {
	c, err := Classify(req(map[string]string{
		"X-WR-Proxy-Mode":    "Proxy",
		"X-WR-API-Key":       "wr_relay",
		"X-WR-Upstream-Host": "anthropic",
		"Authorization":      "Bearer sk-ant-oauth-token",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Mode != ModeProxyAuthed {
		t.Fatalf("mode: want ProxyAuthed, got %v", c.Mode)
	}
	if c.RelayKey != "wr_relay" {
		t.Fatalf("relay key: %q", c.RelayKey)
	}
	if c.UpstreamAuth != "Bearer sk-ant-oauth-token" {
		t.Fatalf("upstream auth: %q", c.UpstreamAuth)
	}
	if c.UpstreamHost != "anthropic" {
		t.Fatalf("upstream host: %q", c.UpstreamHost)
	}
}

func TestClassify_ProxyAnonymous(t *testing.T) {
	c, err := Classify(req(map[string]string{
		"X-WR-Proxy-Mode":    "Proxy",
		"X-WR-Upstream-Host": "openai",
		"Authorization":      "Bearer sk-...",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Mode != ModeProxyAnonymous {
		t.Fatalf("mode: want ProxyAnonymous, got %v", c.Mode)
	}
	if c.RelayKey != "" {
		t.Fatalf("anon should have no relay key: %q", c.RelayKey)
	}
}

func TestClassify_ProxyMissingUpstreamKey(t *testing.T) {
	_, err := Classify(req(map[string]string{
		"X-WR-Proxy-Mode":    "Proxy",
		"X-WR-Upstream-Host": "anthropic",
	}))
	if !errors.Is(err, ErrMissingUpstreamKey) {
		t.Fatalf("want ErrMissingUpstreamKey, got %v", err)
	}
}

func TestClassify_InvalidProxyModeHeader(t *testing.T) {
	_, err := Classify(req(map[string]string{
		"X-WR-Proxy-Mode": "weird",
	}))
	if !errors.Is(err, ErrInvalidProxyMode) {
		t.Fatalf("want ErrInvalidProxyMode, got %v", err)
	}
}

func TestClassify_ClientIP_NoProxyTrust(t *testing.T) {
	// RELAY_TRUSTED_PROXIES unset (test environment), so XFF must be
	// ignored and RemoteAddr wins.
	r := req(map[string]string{
		"X-Forwarded-For": "10.0.0.1",
	})
	c, _ := Classify(r)
	if c.ClientIP != "203.0.113.5" {
		t.Fatalf("client IP: want RemoteAddr (203.0.113.5), got %q", c.ClientIP)
	}
}
