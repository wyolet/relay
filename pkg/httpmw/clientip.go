package httpmw

import (
	"log/slog"
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
		_, cidr, err := net.ParseCIDR(seg)
		if err != nil {
			// A typo silently narrows who is trusted, which shows up much
			// later as an unexplained client IP.
			slog.Warn("httpmw: ignoring unparseable RELAY_TRUSTED_PROXIES entry", "entry", seg, "err", err)
			continue
		}
		trustedProxies = append(trustedProxies, cidr)
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
	// Repeated header lines are one list in order, so the walk spans every line, starting from the last.
	lines := r.Header.Values("X-Forwarded-For")
	for i := len(lines) - 1; i >= 0; i-- {
		for list := lines[i]; list != ""; {
			entry := list
			if j := strings.LastIndexByte(list, ','); j >= 0 {
				entry, list = list[j+1:], list[:j]
			} else {
				list = ""
			}
			ip := net.ParseIP(strings.TrimSpace(entry))
			if ip == nil {
				continue
			}
			if !isTrustedProxy(ip, trusted) {
				return ip.String()
			}
		}
	}
	return remote
}

// FromTrustedProxy reports whether the request's immediate peer is in trusted, so its forwarding headers may be believed.
func FromTrustedProxy(r *http.Request, trusted []*net.IPNet) bool {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	peer := net.ParseIP(remote)
	return peer != nil && isTrustedProxy(peer, trusted)
}

func isTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	for _, cidr := range trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
