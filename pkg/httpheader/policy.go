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

// InboundAllowlist is the set of headers that are permitted to pass through
// inbound to the pipeline. Every header NOT in this list is stripped.
// Matching is case-insensitive. Prefix-style matchers use a trailing "*".
//
// Note: Authorization is retained here so that auth middleware (which runs
// before StripInbound) can still see it. In practice auth middleware removes
// it before StripInbound is called; any residual Authorization is still
// dropped by OutboundAllowlist before the upstream request is made.
var InboundAllowlist = []string{
	"Content-Type",
	"Content-Length",
	"Accept",
	"Accept-Encoding",
	"User-Agent",
	"X-Request-ID",
	"X-Relay-Metadata",
	"X-WR-API-Key",
	"Authorization",
	"Anthropic-Version",
	"Anthropic-Beta",
	"Anthropic-Dangerous-Direct-Browser-Access",
	"X-App",
	"X-Claude-Code-Session-Id",
	"X-Stainless-*",
}

// OutboundAllowlist is the set of headers forwarded on upstream requests.
// Compared to InboundAllowlist:
//   - X-Relay-Metadata is excluded (M4 contract: never leaked to upstream)
//   - Authorization is excluded (provider client injects its own key)
//   - Provider-specific headers are added (e.g. OpenAI-Beta)
var OutboundAllowlist = []string{
	"Content-Type",
	"Content-Length",
	"Accept",
	"Accept-Encoding",
	"User-Agent",
	"X-Request-ID",
	"OpenAI-Beta",
	"Anthropic-Version",
	"Anthropic-Beta",
}

// StripInbound removes every header from h that is not in InboundAllowlist.
// Hop-by-hop headers listed in Connection are also removed (per RFC 7230).
// Returns the same map for chaining.
func StripInbound(h http.Header) http.Header {
	// RFC 7230: remove headers named in Connection value.
	if conn := h.Get("Connection"); conn != "" {
		for _, tok := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for name := range h {
		if !Match(name, InboundAllowlist) {
			h.Del(name)
		}
	}
	return h
}

// StripOutbound removes every header from h that is not in OutboundAllowlist.
// This is used by provider clients to sanitize headers before sending upstream.
// Returns the same map for chaining.
func StripOutbound(h http.Header) http.Header {
	for name := range h {
		if !Match(name, OutboundAllowlist) {
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
