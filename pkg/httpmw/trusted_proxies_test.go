package httpmw

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

// A typo in RELAY_TRUSTED_PROXIES silently narrows who is trusted, which
// surfaces much later as an unexplained client IP.
func TestUnparseableTrustedProxyIsLogged(t *testing.T) {
	t.Setenv("RELAY_TRUSTED_PROXIES", "10.0.0.0/8, not-an-ip, 192.168.1.1")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	trustedProxies = nil
	t.Cleanup(func() { trustedProxies = nil })
	loadTrustedProxies()

	if len(trustedProxies) != 2 {
		t.Fatalf("parsed %d entries, want the two valid ones", len(trustedProxies))
	}
	if !strings.Contains(buf.String(), "not-an-ip") {
		t.Errorf("the rejected entry was not logged: %q", buf.String())
	}
}

func TestFromTrustedProxy(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	trusted := []*net.IPNet{cidr}
	for addr, want := range map[string]bool{"10.1.2.3:443": true, "10.1.2.3": true, "192.0.2.1:443": false, "garbage": false} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		if got := FromTrustedProxy(r, trusted); got != want {
			t.Errorf("FromTrustedProxy(%q) = %v, want %v", addr, got, want)
		}
	}
	if FromTrustedProxy(httptest.NewRequest("GET", "/", nil), nil) {
		t.Error("an empty trusted set trusted the peer")
	}
}
