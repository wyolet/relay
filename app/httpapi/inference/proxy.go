package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/proxy"
	apprl "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/reqid"
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
	slog.Info("proxy: handle entry",
		"request_id", reqid.From(ctx),
		"adapter", string(adapterKind),
		"mode", cls.Mode,
		"upstream_host_hdr", cls.UpstreamHost,
	)

	pm, ok := proxyModeSettings(d.Catalog)
	if !ok || !pm.Enabled {
		d.fireUsageFailure(ctx, "proxy_disabled", "proxy mode is not enabled on this relay")
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "proxy_disabled",
			"proxy mode is not enabled on this relay")
		return
	}
	if cls.Mode == ModeProxyAnonymous && !pm.AllowUnauthenticated {
		d.fireUsageFailure(ctx, "unauthenticated", "anonymous proxy traffic is not enabled on this relay")
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated",
			"anonymous proxy traffic is not enabled on this relay")
		return
	}

	// Resolve upstream host. Two paths:
	//   1. Caller pinned a host via X-WR-Upstream-Host → direct lookup.
	//   2. Header omitted → peek `model` from the body, resolve via
	//      Policy + HostBinding (proxy-authed only — anonymous traffic
	//      has no policy and must still pin a host).
	snap := d.Catalog.Current()
	var (
		host         *apphost.Host
		resolvedPlan *routing.Plan
	)
	if cls.UpstreamHost != "" {
		h, ok := snap.HostByName(cls.UpstreamHost)
		if !ok {
			d.fireUsageFailure(ctx, "unknown_upstream_host", "unknown upstream host "+cls.UpstreamHost)
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "unknown_upstream_host",
				"unknown upstream host "+strconvQuote(cls.UpstreamHost)+"; see GET /v1/proxy/hosts")
			return
		}
		host = h
	} else {
		if cls.Mode != ModeProxyAuthed {
			d.fireUsageFailure(ctx, "missing_upstream_host", "anonymous proxy traffic requires an upstream-host header")
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_host",
				"anonymous proxy traffic requires "+httpheader.HeaderUpstreamHost+" header naming a configured host")
			return
		}
		plan, body, err := resolveProxyHostByPolicy(r, d.Resolver, RelayKeyFromContext(ctx))
		if err != nil {
			d.fireUsageFailure(ctx, "proxy_resolve_error", err.Error())
			mapProxyResolveErr(w, err)
			return
		}
		host = plan.Host
		resolvedPlan = plan
		// Replace the consumed body with a re-readable copy so the
		// forwarder sees the same bytes the customer sent.
		r.Body = io.NopCloser(bytes.NewReader(body))
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
	// hop-by-hop. Proxy.Run re-attaches the caller's UpstreamAuth.
	forwardHdr := r.Header.Clone()
	httpheader.Strip(forwardHdr)

	lc := lifecycle.FromContext(ctx)
	if lc != nil {
		if host != nil {
			lc.HostID = host.Meta.ID
		}
		if rk := RelayKeyFromContext(ctx); rk != nil {
			lc.PolicyID = rk.Spec.PolicyID
		}
		applyPlanIdentity(lc, resolvedPlan) // Plan overrides when present
		if spec := d.Specs.Spec(adapterKind); spec != nil {
			lc.Translator = spec.Translator
		}
	}
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
		Lifecycle:    lc,
	}

	slog.Info("proxy: calling upstream",
		"request_id", reqid.From(ctx),
		"host", host.Meta.Name,
		"base_url", host.Spec.BaseURL,
		"path", r.URL.Path,
	)
	result, err := d.Proxy.Run(ctx, preq)
	if err != nil {
		slog.Warn("proxy: upstream call returned error", "request_id", reqid.From(ctx), "err", err)
		mapProxyErr(w, err)
		return
	}
	slog.Info("proxy: upstream responded; streaming back",
		"request_id", reqid.From(ctx),
		"status", result.Status,
	)
	defer result.Body.Close()

	ForwardUpstreamHeaders(w.Header(), result.Headers)
	w.WriteHeader(result.Status)
	n, copyErr := streamCopy(w, result.Body)
	slog.Info("proxy: stream complete",
		"request_id", reqid.From(ctx),
		"bytes", n,
		"copy_err", copyErr,
	)
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

// proxyMaxBodyPeek caps how many bytes resolveProxyHostByPolicy will
// buffer when peeking the inbound JSON body for its `model` field.
// 1 MiB comfortably exceeds any real chat-completion payload while
// preventing a hostile caller from forcing an unbounded buffer.
const proxyMaxBodyPeek = 1 << 20

// errProxyHostResolve is the typed envelope returned by
// resolveProxyHostByPolicy. mapProxyResolveErr maps each Reason to an
// HTTP envelope.
type errProxyHostResolve struct {
	Reason string
	Detail string
	Model  string // requested model, when known — surfaced on routing rejections
	Inner  error
}

func (e *errProxyHostResolve) Error() string {
	if e.Inner != nil {
		return e.Reason + ": " + e.Detail + ": " + e.Inner.Error()
	}
	return e.Reason + ": " + e.Detail
}

// resolveProxyHostByPolicy reads r.Body once (capped at
// proxyMaxBodyPeek), extracts the `model` field, and runs the catalog
// routing resolver with SkipKeyCheck so proxy mode can reuse the same
// policy + binding logic as normal mode without requiring keypool
// coverage. Returns the resolved Plan AND the buffered body so the
// caller can re-attach it to r.Body before forwarding and seed the
// lifecycle context with model/host/policy attribution.
func resolveProxyHostByPolicy(r *http.Request, resolver *routing.Resolver, rk *relaykey.RelayKey) (*routing.Plan, []byte, error) {
	if rk == nil {
		return nil, nil, &errProxyHostResolve{Reason: "relay_key_required", Detail: "policy-driven host resolution requires an authenticated relay key"}
	}
	limited := io.LimitReader(r.Body, proxyMaxBodyPeek+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, &errProxyHostResolve{Reason: "body_read", Detail: "could not read request body", Inner: err}
	}
	if len(body) > proxyMaxBodyPeek {
		return nil, nil, &errProxyHostResolve{Reason: "body_too_large", Detail: "request body exceeds proxy peek limit"}
	}

	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return nil, body, &errProxyHostResolve{Reason: "body_not_json", Detail: "proxy mode without X-WR-Upstream-Host requires a JSON body carrying a 'model' field", Inner: err}
	}
	if peek.Model == "" {
		return nil, body, &errProxyHostResolve{Reason: "missing_model", Detail: "request body has no 'model' field; cannot resolve upstream host without it"}
	}

	plan, err := resolver.Resolve(routing.Request{
		ModelName:    peek.Model,
		RelayKey:     rk,
		SkipKeyCheck: true,
	})
	if err != nil {
		return nil, body, &errProxyHostResolve{Reason: "routing", Detail: "could not resolve host from policy + model", Model: peek.Model, Inner: err}
	}
	return plan, body, nil
}

func mapProxyResolveErr(w http.ResponseWriter, err error) {
	e, ok := err.(*errProxyHostResolve)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "proxy_resolve", err.Error())
		return
	}
	switch e.Reason {
	case "relay_key_required":
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_relay_key", e.Detail)
	case "body_read":
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "body_read", e.Error())
	case "body_too_large":
		writeAPIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", e.Detail)
	case "body_not_json", "missing_model":
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", e.Reason, e.Detail)
	case "routing":
		// Re-map the inner routing sentinel to the same envelope the
		// normal-mode handler uses, so callers see consistent error codes.
		if e.Inner != nil {
			mapRoutingErr(w, e.Inner, e.Model, "")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "routing", e.Detail)
	default:
		writeAPIError(w, http.StatusInternalServerError, "server_error", "proxy_resolve", e.Error())
	}
}
