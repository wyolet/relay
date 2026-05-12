// Package mode classifies inbound requests by their proxy mode and extracts the
// relevant credentials from request headers.
//
// Three modes are defined:
//   - ModeNormal      — default; Relay key required; no X-WR-Proxy-Mode or "No-Proxy"
//   - ModeProxyAuthed — X-WR-Proxy-Mode: Proxy + Relay key + provider key (Authorization)
//   - ModeProxyAnonymous — X-WR-Proxy-Mode: Proxy + provider key (Authorization) only
//
// In proxy mode the provider key is always the raw bearer token from
// Authorization: Bearer <token>. The Relay key travels in X-WR-API-Key only
// (Authorization is reserved for the provider key).
//
// IP extraction trusts X-Forwarded-For when RELAY_TRUSTED_PROXIES is set
// (comma-separated CIDRs). When unset RemoteAddr is used verbatim.
package mode

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
)

// Mode classifies the proxy mode of an inbound request.
type Mode int

const (
	ModeUnknown         Mode = iota
	ModeNormal               // default; no X-WR-Proxy-Mode or "No-Proxy"; Relay key required
	ModeProxyAuthed          // X-WR-Proxy-Mode: Proxy + Relay key + provider key
	ModeProxyAnonymous       // X-WR-Proxy-Mode: Proxy + provider key only
)

// Classification carries the result of Classify.
type Classification struct {
	Mode        Mode
	RelayKey    string // empty in ModeProxyAnonymous
	ProviderKey string // empty in ModeNormal; raw bearer-stripped token
	ClientIP    string // resolved from XFF / RemoteAddr
}

// Sentinel errors returned by Classify. HTTP callers should map these to 400.
var (
	ErrInvalidProxyModeHeader = errors.New("mode: invalid X-WR-Proxy-Mode value")
	ErrMissingProviderKey     = errors.New("mode: Proxy mode requires Authorization: Bearer <provider-key>")
)

// Classify extracts the request mode, credentials, and client IP from r.
// Returns an error only for unambiguously invalid inputs (bad header value,
// missing provider key in Proxy mode). The caller must map errors to HTTP
// responses — this package has no net/http response writing.
func Classify(r *http.Request) (Classification, error) {
	raw := r.Header.Get("X-WR-Proxy-Mode")
	proxyMode := false
	switch raw {
	case "Proxy":
		proxyMode = true
	case "No-Proxy", "":
		// normal
	default:
		return Classification{}, ErrInvalidProxyModeHeader
	}

	if proxyMode {
		// Authorization is the provider key.
		authz := r.Header.Get("Authorization")
		providerKey := bearerToken(authz)
		if providerKey == "" {
			return Classification{}, ErrMissingProviderKey
		}
		relayKey := r.Header.Get("X-WR-API-Key") // NOT Authorization
		if relayKey != "" {
			return Classification{
				Mode:        ModeProxyAuthed,
				RelayKey:    relayKey,
				ProviderKey: providerKey,
				ClientIP:    extractIP(r),
			}, nil
		}
		return Classification{
			Mode:        ModeProxyAnonymous,
			ProviderKey: providerKey,
			ClientIP:    extractIP(r),
		}, nil
	}

	// Normal mode: relay key lookup order — X-WR-API-Key, x-api-key, Authorization Bearer.
	relayKey := r.Header.Get("X-WR-API-Key")
	if relayKey == "" {
		relayKey = r.Header.Get("x-api-key")
	}
	if relayKey == "" {
		relayKey = bearerToken(r.Header.Get("Authorization"))
	}
	return Classification{
		Mode:     ModeNormal,
		RelayKey: relayKey,
		ClientIP: extractIP(r),
	}, nil
}

// bearerToken strips "Bearer " prefix from an Authorization header value.
// Returns "" when the header is empty or uses a different scheme.
func bearerToken(raw string) string {
	if !strings.HasPrefix(raw, "Bearer ") {
		return ""
	}
	return raw[len("Bearer "):]
}

// trustedProxies is parsed once from RELAY_TRUSTED_PROXIES env on init.
var trustedProxies []*net.IPNet

func init() {
	env := os.Getenv("RELAY_TRUSTED_PROXIES")
	if env == "" {
		return
	}
	for _, seg := range strings.Split(env, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		// Accept bare IPs as /32 or /128.
		if !strings.Contains(seg, "/") {
			if strings.Contains(seg, ":") {
				seg += "/128"
			} else {
				seg += "/32"
			}
		}
		_, cidr, err := net.ParseCIDR(seg)
		if err == nil {
			trustedProxies = append(trustedProxies, cidr)
		}
	}
}

// extractIP returns the best-guess client IP. When trustedProxies is configured
// and the direct peer matches a trusted range, the leftmost non-trusted IP in
// X-Forwarded-For is used. Otherwise RemoteAddr (without port) is returned.
func extractIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	if len(trustedProxies) == 0 {
		return remoteIP
	}

	peerIP := net.ParseIP(remoteIP)
	if peerIP == nil || !isTrusted(peerIP) {
		return remoteIP
	}

	// Peer is trusted — walk XFF right-to-left, skip trusted entries.
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue
		}
		if !isTrusted(ip) {
			return ip.String()
		}
	}
	return remoteIP
}

func isTrusted(ip net.IP) bool {
	for _, cidr := range trustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
