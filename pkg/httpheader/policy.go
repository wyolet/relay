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

// SensitiveInbound is the set of headers Relay strips from inbound
// HTTP requests before the request enters the pipeline. They are
// either credentials (must never be forwarded), Relay-reserved
// (use specific X-Relay-* fields instead), or provider-private
// (X-OpenAI-*, etc.).
//
// Matching is case-insensitive. Prefix-style matchers are denoted
// by a trailing "*" (e.g., "X-OpenAI-*").
var SensitiveInbound = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-OpenAI-*",
}

// StripInbound removes credential and provider-private headers from
// h in place. Hop-by-hop headers listed in Connection are also
// removed (per RFC 7230). Returns the same map for chaining.
func StripInbound(h http.Header) http.Header {
	// RFC 7230: remove headers named in Connection value.
	if conn := h.Get("Connection"); conn != "" {
		for _, tok := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for name := range h {
		if Match(name, SensitiveInbound) {
			h.Del(name)
		}
	}
	return h
}

// SanitizeUpstreamResponse strips hop-by-hop headers from the
// upstream response before they are propagated back to the caller.
// Modifies h in place. Returns the same map.
func SanitizeUpstreamResponse(h http.Header) http.Header {
	// RFC 7230: remove headers named in Connection value first.
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
// or prefix-style with a trailing "*" (e.g., "X-OpenAI-*").
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
