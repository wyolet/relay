package httpmw

import (
	"bytes"
	"log/slog"
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
