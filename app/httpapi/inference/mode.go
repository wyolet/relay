package inference

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/wyolet/relay/pkg/httpheader"
)

// Mode classifies the proxy mode of an inbound inference request.
type Mode int

const (
	ModeUnknown Mode = iota
	// ModeNormal — default; no X-WR-Proxy-Mode; relay key authenticates
	// against a Policy and the upstream key comes from the keypool.
	ModeNormal
	// ModeProxyAuthed — X-WR-Proxy-Mode: Proxy + relay key in X-WR-API-Key
	// + caller-supplied upstream key in Authorization. Logged against the
	// relay key; subject to the inference-api-proxy system rate limit.
	ModeProxyAuthed
	// ModeProxyAnonymous — X-WR-Proxy-Mode: Proxy + caller-supplied
	// upstream key in Authorization, no relay key. Subject to the
	// inference-api-proxy-anonymous per-IP rate limit. Gated by
	// Settings.ProxyMode.AllowUnauthenticated.
	ModeProxyAnonymous
)

// Classification carries the result of Classify. Stashed on ctx by the
// classifier middleware and read by downstream handlers / pipeline /
// proxy. The classifier is pure — it does not touch the catalog snapshot
// or any settings; gating decisions happen later.
type Classification struct {
	Mode Mode
	// RelayKey is the raw inbound relay-key token. Empty in
	// ModeProxyAnonymous. In ModeNormal pulled from X-WR-API-Key first,
	// then Authorization (Bearer). In ModeProxyAuthed pulled from
	// X-WR-API-Key only.
	RelayKey string
	// UpstreamAuth is the verbatim Authorization header value (including
	// the "Bearer " prefix when present) supplied by the caller for proxy
	// mode. The proxy forwarder re-attaches it on the outbound request.
	// Empty in ModeNormal.
	UpstreamAuth string
	// UpstreamHost is the X-WR-Upstream-Host slug naming a configured
	// Host row. Empty in ModeNormal. Required in proxy mode; the proxy
	// dispatcher rejects missing/unknown values.
	UpstreamHost string
	// ClientIP is the resolved client address — XFF-walked when the peer
	// matches RELAY_TRUSTED_PROXIES, RemoteAddr otherwise.
	ClientIP string
}

// Sentinel errors returned by Classify. The middleware maps these to 400.
var (
	ErrInvalidProxyMode   = errors.New("inference: invalid X-WR-Proxy-Mode value")
	ErrMissingUpstreamKey = errors.New("inference: proxy mode requires Authorization: Bearer <upstream-key>")
)

// Classify extracts the mode, credentials, and client IP from r. Pure;
// no ctx writes, no snapshot reads. Returns an error only for shape-
// invalid inputs (bad header value, missing upstream key in proxy mode);
// the caller maps errors to HTTP responses.
func Classify(r *http.Request) (Classification, error) {
	raw := r.Header.Get(httpheader.HeaderProxyMode)
	proxy := false
	switch raw {
	case httpheader.ProxyModeValueProxy:
		proxy = true
	case "", "No-Proxy":
		// normal mode
	default:
		return Classification{}, ErrInvalidProxyMode
	}

	if proxy {
		authz := r.Header.Get("Authorization")
		if authz == "" {
			return Classification{}, ErrMissingUpstreamKey
		}
		relay := r.Header.Get(httpheader.HeaderRelayAPIKey)
		mode := ModeProxyAnonymous
		if relay != "" {
			mode = ModeProxyAuthed
		}
		return Classification{
			Mode:         mode,
			RelayKey:     relay,
			UpstreamAuth: authz,
			UpstreamHost: r.Header.Get(httpheader.HeaderUpstreamHost),
			ClientIP:     extractIP(r),
		}, nil
	}

	// Normal mode: relay key lookup order — X-WR-API-Key, then
	// Authorization Bearer, then x-api-key (Anthropic-SDK convention so
	// clients pointed at /v1/messages with stock SDK auth Just Work).
	relay := r.Header.Get(httpheader.HeaderRelayAPIKey)
	if relay == "" {
		relay = bearer(r.Header.Get("Authorization"))
	}
	if relay == "" {
		relay = r.Header.Get("x-api-key")
	}
	return Classification{
		Mode:     ModeNormal,
		RelayKey: relay,
		ClientIP: extractIP(r),
	}, nil
}

// ctxClassificationT is the context-value key for the request mode
// classification.
type ctxClassificationT struct{}

// WithClassification returns a child ctx carrying c.
func WithClassification(ctx context.Context, c Classification) context.Context {
	return context.WithValue(ctx, ctxClassificationT{}, c)
}

// ClassificationFrom returns the mode classification from ctx. The zero
// Classification (Mode == ModeUnknown) is returned if no classifier ran.
func ClassificationFrom(ctx context.Context) Classification {
	if v, ok := ctx.Value(ctxClassificationT{}).(Classification); ok {
		return v
	}
	return Classification{}
}

// ClassifyMiddleware runs Classify once per request and stashes the
// result on ctx. Shape-invalid requests get a 400 envelope; everything
// else falls through to the next handler. Gating (Settings.ProxyMode
// flags, unknown-slug rejection) lives downstream so this middleware
// stays snapshot-free.
func ClassifyMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := Classify(r)
			if err != nil {
				writeClassifyErr(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithClassification(r.Context(), c)))
		})
	}
}

func writeClassifyErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"bad_request","message":"` + err.Error() + `"}}`))
}

// --- client IP resolution ---

var (
	trustedProxiesOnce sync.Once
	trustedProxies     []*net.IPNet
)

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

func extractIP(r *http.Request) string {
	trustedProxiesOnce.Do(loadTrustedProxies)

	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	if len(trustedProxies) == 0 {
		return remote
	}
	peer := net.ParseIP(remote)
	if peer == nil || !isTrustedProxy(peer) {
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
		if !isTrustedProxy(ip) {
			return ip.String()
		}
	}
	return remote
}

func isTrustedProxy(ip net.IP) bool {
	for _, cidr := range trustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
