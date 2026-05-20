package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/proxy"
	apprl "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/httpheader"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// System rate-limit slugs from config/ratelimits/system.yaml. Proxy mode
// only consults the proxy-specific buckets; normal-mode buckets are
// orthogonal.
const (
	systemRLProxyAuthed = "inference-api-proxy"
	systemRLProxyAnon   = "inference-api-proxy-anonymous"
)

// handleProxy is the shared proxy-mode flow called by /v1/chat/completions
// and /v1/messages. adapterKind pins the token extractor (and is used in
// future per-host adapter-mismatch checks).
func handleProxy(d Deps, w http.ResponseWriter, r *http.Request, adapterKind adapters.Name) {
	ctx := r.Context()
	cls := ClassificationFrom(ctx)

	pm, ok := proxyModeSettings(d.Catalog)
	if !ok || !pm.Enabled {
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "proxy_disabled",
			"proxy mode is not enabled on this relay")
		return
	}
	if cls.Mode == ModeProxyAnonymous && !pm.AllowUnauthenticated {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated",
			"anonymous proxy traffic is not enabled on this relay")
		return
	}

	// Resolve upstream host slug → Host row.
	if cls.UpstreamHost == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_host",
			"proxy mode requires "+httpheader.HeaderUpstreamHost+" header naming a configured host")
		return
	}
	snap := d.Catalog.Current()
	host, ok := snap.HostByName(cls.UpstreamHost)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "unknown_upstream_host",
			"unknown upstream host "+strconvQuote(cls.UpstreamHost)+"; see GET /v1/proxy/hosts")
		return
	}

	// Resolve system rate-limit bucket + subject for this mode.
	bucketName := systemRLProxyAuthed
	subject := relayKeyHashSubject(ctx)
	if cls.Mode == ModeProxyAnonymous {
		bucketName = systemRLProxyAnon
		subject = cls.ClientIP
	}
	rules := resolveSystemRules(snap, bucketName, subject)

	// Strip relay-internal headers before forward — Authorization, X-WR-*,
	// X-Relay-Metadata, hop-by-hop. Proxy.Run re-attaches the caller's
	// UpstreamAuth.
	forwardHdr := r.Header.Clone()
	httpheader.Strip(forwardHdr)

	preq := &proxy.Request{
		Method:       r.Method,
		Path:         r.URL.Path,
		Body:         r.Body,
		Headers:      forwardHdr,
		HostBaseURL:  host.Spec.BaseURL,
		UpstreamAuth: cls.UpstreamAuth,
		RateScope:    subject,
		Rules:        rules,
		Extractor:    extractorFor(d, adapterKind),
	}

	result, err := d.Proxy.Run(ctx, preq)
	if err != nil {
		mapProxyErr(w, err)
		return
	}
	defer result.Body.Close()

	for k, vs := range result.Headers {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(result.Status)
	_, _ = io.Copy(w, result.Body)
}

// extractorFor returns the per-shape token extractor used by proxy
// post-flight. Reuses d.Adapters since pipeline.Adapter implementations
// satisfy proxy.TokenExtractor by virtue of having ExtractTokens.
func extractorFor(d Deps, name adapters.Name) proxy.TokenExtractor {
	if a, ok := d.Adapters[name]; ok {
		return a
	}
	return nil
}

func proxyModeSettings(cat *appcatalog.Catalog) (*settings.ProxyMode, bool) {
	v, ok := cat.Setting(settings.SectionProxyMode)
	if !ok {
		return nil, false
	}
	pm, ok := v.(*settings.ProxyMode)
	return pm, ok
}

func resolveSystemRules(snap *appcatalog.Snapshot, bucketName, subject string) []pkgratelimit.Rule {
	if subject == "" {
		return nil
	}
	rl, ok := snap.RateLimitByName(bucketName)
	if !ok {
		return nil
	}
	return apprl.ResolveWithScope("proxy", subject, rl)
}

// relayKeyHashSubject returns the SHA-256 hash of the relay-key token
// from ctx, suitable as a per-key rate-limit subject. Empty when no
// relay key is on ctx (proxy-anonymous path).
func relayKeyHashSubject(ctx context.Context) string {
	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		return ""
	}
	// The snapshot stores Spec.KeyHash already; reuse it for stability.
	if rk.Spec.KeyHash != "" {
		return rk.Spec.KeyHash
	}
	// Fallback shouldn't happen — RelayKeyByHash matched by hash.
	cls := ClassificationFrom(ctx)
	sum := sha256.Sum256([]byte(cls.RelayKey))
	return hex.EncodeToString(sum[:])
}

func mapProxyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, proxy.ErrNoUpstreamAuth):
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_auth",
			"proxy mode requires Authorization: Bearer <upstream-key>")
	default:
		// Limiter exceeded → 429 mapping happens at this layer too. The
		// pkg/ratelimit ExceededError carries Retry-After info; here we
		// keep it simple.
		var ex *pkgratelimit.ExceededError
		if errors.As(err, &ex) {
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit",
				ex.Error())
			return
		}
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", err.Error())
	}
}

// strconvQuote wraps strconv.Quote without importing strconv at the
// top of every file in this package. Inline because the only use is in
// error messages.
func strconvQuote(s string) string { return "\"" + s + "\"" }
