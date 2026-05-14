package httpheader

import (
	"net/http"
	"regexp"
	"strings"
)

var (
	reIP  = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?`)
	reURL = regexp.MustCompile(`https?://[^\s]+`)
)

// Relay-internal control headers. Read at the inference edge by the
// mode classifier; never forwarded upstream.
const (
	HeaderRelayAPIKey   = "X-WR-API-Key"
	HeaderProxyMode     = "X-WR-Proxy-Mode"
	HeaderUpstreamHost  = "X-WR-Upstream-Host"
	ProxyModeValueProxy = "Proxy"
)

// SafeUpstreamError returns a user-safe error message for an upstream failure,
// redacting URLs, IP addresses, and other internal details.
// providerName is the public provider identifier (e.g. "openai").
func SafeUpstreamError(providerName string, err error) string {
	msg := err.Error()
	if reURL.MatchString(msg) || reIP.MatchString(msg) {
		return providerName + ": upstream connection failed"
	}
	return providerName + ": " + msg
}

// HopByHop is the canonical RFC 7230 hop-by-hop header set.
var HopByHop = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// StripDenylist is the negative strip set applied at the inference edge
// after the mode classifier has captured the relay-internal headers it
// needs. Everything else passes through to upstream — the relay does
// not police which vendor-specific headers callers send.
//
// Authorization is denylisted at the edge for both modes:
//   - Normal mode: relay-key value, consumed by auth; adapter injects
//     the upstream key on the outbound request.
//   - Proxy mode: caller's upstream key, captured by the classifier;
//     the proxy forwarder re-attaches it on the outbound request.
//
// Either way, ctx is the source of truth post-strip; nothing reads the
// raw inbound Authorization downstream.
//
// Matching is case-insensitive. Trailing "*" is a prefix match.
var StripDenylist = []string{
	"Authorization",
	"Cookie",
	"X-WR-*",
	"X-Relay-Metadata",
}

// Strip removes relay-internal control headers and hop-by-hop headers
// from h. Applied once at the inference edge, after the mode classifier
// has captured what it needs into ctx. Returns the same map for chaining.
func Strip(h http.Header) http.Header {
	// RFC 7230: remove headers named in Connection value.
	if conn := h.Get("Connection"); conn != "" {
		for _, tok := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for _, name := range HopByHop {
		h.Del(name)
	}
	for name := range h {
		if Match(name, StripDenylist) {
			h.Del(name)
		}
	}
	return h
}

// SanitizeUpstreamResponse strips hop-by-hop headers from the
// upstream response before they are propagated back to the caller.
// Modifies h in place. Returns the same map.
func SanitizeUpstreamResponse(h http.Header) http.Header {
	if conn := h.Get("Connection"); conn != "" {
		for _, tok := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for _, name := range HopByHop {
		h.Del(name)
	}
	return h
}

// Match reports whether name matches any pattern in patterns,
// case-insensitively. Patterns may be exact (e.g., "Authorization")
// or prefix-style with a trailing "*" (e.g., "X-WR-*").
func Match(name string, patterns []string) bool {
	lower := strings.ToLower(name)
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			prefix := strings.ToLower(p[:len(p)-1])
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		} else {
			if lower == strings.ToLower(p) {
				return true
			}
		}
	}
	return false
}
