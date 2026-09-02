package httpmw

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

var (
	trustedProxiesOnce sync.Once
	trustedProxies     []*net.IPNet
)

// TrustedProxies parses RELAY_TRUSTED_PROXIES (comma-separated IPs or
// CIDRs) once per process. Empty means no proxy is trusted, so ClientIP
// ignores forwarding headers entirely.
func TrustedProxies() []*net.IPNet {
	trustedProxiesOnce.Do(loadTrustedProxies)
	return trustedProxies
}

func loadTrustedProxies() {
	env := os.Getenv("RELAY_TRUSTED_PROXIES")
	if env == "" {
		return
	}
	for _, seg := range strings.Split(env, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if !strings.Contains(seg, "/") {
			if strings.Contains(seg, ":") {
				seg += "/128"
			} else {
				seg += "/32"
			}
		}
		if _, cidr, err := net.ParseCIDR(seg); err == nil {
			trustedProxies = append(trustedProxies, cidr)
		}
	}
}

// ClientIP resolves the caller's address: the rightmost X-Forwarded-For hop
// that is not itself a trusted proxy when the peer is one, RemoteAddr
// otherwise. Walking from the right is what makes the result unspoofable —
// a client-supplied prefix can never win.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	if len(trusted) == 0 {
		return remote
	}
	peer := net.ParseIP(remote)
	if peer == nil || !isTrustedProxy(peer, trusted) {
		return remote
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue
		}
		if !isTrustedProxy(ip, trusted) {
			return ip.String()
		}
	}
	return remote
}

func isTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	for _, cidr := range trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
