package httpheader

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// inbound is a request as it reaches the edge: credentials and relay directives beside what the upstream must see, values included.
func inbound() http.Header {
	return http.Header{
		"Authorization":            {"Bearer relay-key"},
		"X-Api-Key":                {"relay-key"},
		"Cookie":                   {"sid=abc"},
		"X-Wr-Api-Key":             {"wr_foo"},
		"X-Wr-Proxy-Mode":          {"Proxy"},
		"X-Wr-Upstream-Host":       {"vendor"},
		HeaderClient:               {"claude-code"},
		"X-Wr-Anything-New":        {"1"},
		"Connection":               {"keep-alive, x-custom , X-Other"},
		"X-Custom":                 {"listed in Connection"},
		"X-Other":                  {"listed in Connection"},
		"Keep-Alive":               {"timeout=5"},
		"Proxy-Authenticate":       {"Basic"},
		"Proxy-Authorization":      {"Basic bar"},
		"Te":                       {"trailers"},
		"Transfer-Encoding":        {"chunked"},
		"Upgrade":                  {"websocket"},
		"Content-Type":             {"application/json"},
		"Accept":                   {"*/*", "text/plain"},
		"User-Agent":               {"relay-test"},
		"Anthropic-Beta":           {"prompt-caching-2024-07-31"},
		"X-Stainless-Lang":         {"js"},
		"X-Claude-Code-Session-Id": {"sess_123"},
		"X-Wrong-Prefix":           {"kept: not X-Wr-"},
		"X-Api-Key-Id":             {"kept: not the key itself"},
	}
}

func TestStrip(t *testing.T) {
	h := inbound()
	want := http.Header{
		"Content-Type":             {"application/json"},
		"Accept":                   {"*/*", "text/plain"},
		"User-Agent":               {"relay-test"},
		"Anthropic-Beta":           {"prompt-caching-2024-07-31"},
		"X-Stainless-Lang":         {"js"},
		"X-Claude-Code-Session-Id": {"sess_123"},
		"X-Wrong-Prefix":           {"kept: not X-Wr-"},
		"X-Api-Key-Id":             {"kept: not the key itself"},
	}
	got := Strip(h)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("headers forwarded upstream (-want +got):\n%s", diff)
	}
	if len(h) != len(want) {
		t.Error("Strip returned a copy; callers rely on it stripping in place")
	}
}

// An upstream response loses its hop-by-hop headers and nothing else: relay's own credentials never appear in one.
func TestSanitizeUpstreamResponse(t *testing.T) {
	h := inbound()
	want := inbound()
	for _, name := range []string{"Connection", "X-Custom", "X-Other", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Transfer-Encoding", "Upgrade"} {
		delete(want, name)
	}
	if diff := cmp.Diff(want, SanitizeUpstreamResponse(h)); diff != "" {
		t.Errorf("headers passed to the caller (-want +got):\n%s", diff)
	}
}

func TestStripRemovesTheTrailerHeader(t *testing.T) {
	for name, strip := range map[string]func(http.Header) http.Header{"Strip": Strip, "SanitizeUpstreamResponse": SanitizeUpstreamResponse} {
		if got := strip(http.Header{"Trailer": {"X-Checksum"}}); len(got) != 0 {
			t.Errorf("%s kept %v", name, got)
		}
	}
}

func TestStripHonoursEveryConnectionLine(t *testing.T) {
	got := Strip(http.Header{"Connection": {"close", "X-Hop"}, "X-Hop": {"1"}, "Accept": {"*/*"}})
	if diff := cmp.Diff(http.Header{"Accept": {"*/*"}}, got); diff != "" {
		t.Errorf("headers forwarded upstream (-want +got):\n%s", diff)
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		want     bool
	}{
		{"authorization", []string{"Authorization"}, true},
		{"AUTHORIZATION", []string{"authorization"}, true},
		{"Authorizations", []string{"Authorization"}, false},
		{"Authorizatio", []string{"Authorization"}, false},
		{"x-wr-api-key", []string{"X-WR-*"}, true},
		{"X-Wr-", []string{"X-WR-*"}, true},
		{"X-Wr", []string{"X-WR-*"}, false},
		{"x-other", []string{"X-WR-*"}, false},
		{"anything", []string{"*"}, true},
		{"", []string{"*"}, true},
		{"", []string{"X-WR-*", "Cookie"}, false},
		{"Cookie", nil, false},
		{"Cookie", []string{"X-WR-*", "Cookie"}, true},
	} {
		if got := Match(tc.name, tc.patterns); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.name, tc.patterns, got, tc.want)
		}
	}
}

// matchLower lower-cases both sides before comparing: the reference result Match must agree with.
func matchLower(name string, patterns []string) bool {
	lower := strings.ToLower(name)
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(lower, strings.ToLower(p[:len(p)-1])) {
				return true
			}
		} else if lower == strings.ToLower(p) {
			return true
		}
	}
	return false
}

func TestMatch_AgreesWithLowercasedComparison(t *testing.T) {
	names := []string{
		"", "X", "x-wr-", "X-WR-", "X-Wr-Api-Key", "x-wr-api-key", "X-WR-API-KEY", "X-Wr", "x-w",
		"Authorization", "authorization", "AUTHORIZATION", "Authorizatio", "Authorizations",
		"X-Api-Key", "x-api-key", "X-API-KEY", "Cookie", "cOOKIE", "Set-Cookie",
		"Retry-After", "retry-after", "X-Request-Id", "x-request-id", "X-Request-ID", "X-Should-Retry",
		"Content-Type", "content-type", "Content-Length",
		"Anthropic-Ratelimit-Requests-Limit", "anthropic-ratelimit-tokens-reset", "Anthropic-Ratelimit",
		"X-Ratelimit-Remaining-Requests", "x-ratelimit-limit-tokens", "X-Ratelimit",
		"X-Stainless-Lang", "X-Claude-Code-Session-Id", "Accept-Encoding",
	}
	for _, patterns := range [][]string{StripDenylist, UpstreamErrorAllowlist, HopByHop, {"*"}, {"X-WR-*", "x-api-key"}} {
		for _, name := range names {
			if got, want := Match(name, patterns), matchLower(name, patterns); got != want {
				t.Errorf("Match(%q, %q) = %v, want %v", name, patterns, got, want)
			}
		}
	}
}

func TestMatch_DoesNotAllocate(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		Match("X-Stainless-Package-Version", StripDenylist)
		Match("x-wr-api-key", StripDenylist)
		Match("Anthropic-Ratelimit-Tokens-Reset", UpstreamErrorAllowlist)
	})
	if allocs != 0 {
		t.Errorf("Match allocates %v times per run, want 0", allocs)
	}
}

func TestHeaderConstantsAreCanonical(t *testing.T) {
	names := append([]string{
		HeaderRelayAPIKey, HeaderProxyMode, HeaderUpstreamHost, HeaderUsage, HeaderEventTime,
		HeaderRequestTags, HeaderClient, HeaderWarnings, HeaderTokenCount, HeaderContentCapture,
		HeaderShouldRetry,
	}, HopByHop...)
	for _, name := range names {
		if canonical := http.CanonicalHeaderKey(name); canonical != name {
			t.Errorf("%q is not canonical, want %q: Header.Get/Set/Del would re-canonicalize it on every call", name, canonical)
		}
	}
}

// An error that names an address or URL is replaced whole; any other is passed on behind the provider's name.
func TestSafeUpstreamError(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"dial tcp 192.168.0.109:443: connect: connection refused", "vendor: upstream connection failed"},
		{"dial tcp 10.0.0.1: i/o timeout", "vendor: upstream connection failed"},
		{`Post "https://api.vendor.example/v1/foo": dial tcp: connection refused`, "vendor: upstream connection failed"},
		{"redirected to http://internal/x", "vendor: upstream connection failed"},
		{"dial tcp 2001:db8:1:2:3:4:5:6: i/o timeout", "vendor: upstream connection failed"},
		{"listen tcp [::]:443: bind: address already in use", "vendor: upstream connection failed"},
		{"stream reset at 12:30:45", "vendor: stream reset at 12:30:45"},
		{"context deadline exceeded", "vendor: context deadline exceeded"},
		{"unexpected EOF", "vendor: unexpected EOF"},
		{"", "vendor: "},
	} {
		if got := SafeUpstreamError("vendor", errors.New(tc.err)); got != tc.want {
			t.Errorf("SafeUpstreamError(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestSafeUpstreamErrorRedactsIPv6(t *testing.T) {
	const in = "dial tcp [fd00::1234]:443: connect: connection refused"
	if got := SafeUpstreamError("vendor", errors.New(in)); got != "vendor: upstream connection failed" {
		t.Errorf("SafeUpstreamError(%q) = %q, want the address replaced", in, got)
	}
}
