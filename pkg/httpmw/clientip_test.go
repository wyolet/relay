package httpmw

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func cidrs(t *testing.T, list ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, s := range list {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// A bare address is a single host; an entry that does not parse is dropped and logged, since a typo silently narrows who is trusted.
func TestLoadTrustedProxies(t *testing.T) {
	t.Setenv("RELAY_TRUSTED_PROXIES", "10.0.0.0/8, not-an-ip, 192.168.1.1,, ::1 ,2001:db8::/32,10.0.0.0/33")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	trustedProxies = nil
	t.Cleanup(func() { trustedProxies = nil })

	loadTrustedProxies()

	var got []string
	for _, n := range trustedProxies {
		got = append(got, n.String())
	}
	if diff := cmp.Diff([]string{"10.0.0.0/8", "192.168.1.1/32", "::1/128", "2001:db8::/32"}, got); diff != "" {
		t.Errorf("trusted proxies (-want +got):\n%s", diff)
	}
	logged := buf.String()
	if strings.Count(logged, "ignoring unparseable RELAY_TRUSTED_PROXIES entry") != 2 || !strings.Contains(logged, "entry=not-an-ip/32") || !strings.Contains(logged, "entry=10.0.0.0/33") {
		t.Errorf("want one warning per rejected entry, naming it; log:\n%s", logged)
	}
}

func TestClientIP(t *testing.T) {
	trusted := cidrs(t, "10.0.0.0/8", "192.168.1.1/32", "::1/128")
	for _, tc := range []struct {
		name      string
		trusted   []*net.IPNet
		remote    string
		forwarded []string
		want      string
		fromProxy bool
	}{
		{"no trusted proxies: the peer, whatever it claims", nil, "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9", false},
		{"untrusted peer: its header is ignored", trusted, "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9", false},
		{"trusted peer without a header", trusted, "10.0.0.1:443", nil, "10.0.0.1", true},
		{"trusted peer forwards one client", trusted, "10.0.0.1:443", []string{"203.0.113.7"}, "203.0.113.7", true},
		{"a client-supplied entry left of the proxy's is ignored", trusted, "10.0.0.1:443", []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7", true},
		{"trusted hops are skipped from the right", trusted, "10.0.0.1:443", []string{"6.6.6.6,203.0.113.7 , 192.168.1.1, 10.9.9.9"}, "203.0.113.7", true},
		{"every hop trusted: the peer", trusted, "10.0.0.1:443", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.1", true},
		{"an unparseable entry is skipped", trusted, "10.0.0.1:443", []string{"203.0.113.7, unknown"}, "203.0.113.7", true},
		{"trusted hops are skipped across header lines", trusted, "10.0.0.1:443", []string{"6.6.6.6, 203.0.113.7", "10.9.9.9", ""}, "203.0.113.7", true},
		{"IPv6 peer and client", trusted, "[::1]:443", []string{"2001:DB8::1"}, "2001:db8::1", true},
		{"peer address without a port", trusted, "10.0.0.1", []string{"203.0.113.7"}, "203.0.113.7", true},
		{"peer address that is no IP", trusted, "garbage", []string{"203.0.113.7"}, "garbage", false},
		{"host trusted by exact address only", trusted, "192.168.1.2:443", []string{"203.0.113.7"}, "192.168.1.2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			r.Header["X-Forwarded-For"] = tc.forwarded
			if got := ClientIP(r, tc.trusted); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
			if got := FromTrustedProxy(r, tc.trusted); got != tc.fromProxy {
				t.Errorf("FromTrustedProxy = %v, want %v", got, tc.fromProxy)
			}
		})
	}
}

func TestClientIPReadsEveryForwardedHeaderLine(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:443"
	r.Header["X-Forwarded-For"] = []string{"6.6.6.6", "203.0.113.7"}
	if got := ClientIP(r, cidrs(t, "10.0.0.0/8")); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7", got)
	}
}
