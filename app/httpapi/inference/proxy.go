package inference

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	apprl "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/auth/apikey"
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

// handleProxy is the shared proxy-mode flow called by /openai/v1/chat/completions
// and /anthropic/v1/messages. adapterKind pins the token extractor (and is used in
// future per-host adapter-mismatch checks). modelName is the body's model, as
// the shape route held it; r.Body is the scannedBody streaming the rest.
//
// The proxy-mode setting is enforced before this runs: by PrincipalMiddleware, and per frame on /v1/ws.
func handleProxy(d Deps, w http.ResponseWriter, r *http.Request, adapterKind adapters.Name, modelName string) {
	ctx := r.Context()
	cls := ClassificationFrom(ctx)
	slog.Debug("proxy: handle entry",
		"request_id", reqid.From(ctx),
		"adapter", string(adapterKind),
		"mode", cls.Mode,
		"upstream_host_hdr", cls.UpstreamHost,
	)
	reqBody, ok := r.Body.(*scannedBody)
	if !ok {
		// A caller that read the body itself: nothing reaches the upstream unscanned.
		var rej *bodyError
		if reqBody, rej = scanReadBody(r.Body); rej != nil {
			d.fireUsageFailure(ctx, rej.code, rej.msg)
			writeAPIError(w, rej.status, "invalid_request_error", rej.code, rej.msg)
			return
		}
	}

	// Resolve upstream host. Two paths:
	//   1. Caller pinned a host via X-WR-Upstream-Host → direct lookup.
	//   2. Header omitted → resolve the body's model via Policy +
	//      HostBinding (proxy-authed only — anonymous traffic has no
	//      policy and must still pin a host).
	snap := SnapshotFrom(ctx)
	if snap == nil {
		// Anonymous proxy: no credential middleware ran, so nothing pinned
		// a snapshot for this request.
		snap = d.Catalog.Current()
	}
	lc := lifecycle.FromContext(ctx)
	var (
		host         *apphost.Host
		resolvedPlan *routing.Plan
	)
	if cls.UpstreamHost != "" {
		h, ok := snap.HostByName(cls.UpstreamHost)
		kind := "unknown_upstream_host"
		if ok && !proxyPinAllowed(snap, PrincipalFrom(ctx), h) {
			// Answered as unknown, matching its absence from /v1/proxy/hosts.
			ok, kind = false, "upstream_host_not_granted"
		}
		if !ok {
			d.fireUsageFailure(ctx, kind, "unknown upstream host "+cls.UpstreamHost)
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "unknown_upstream_host",
				"unknown upstream host "+strconvQuote(cls.UpstreamHost)+"; see GET /v1/proxy/hosts", "reason", kind)
			return
		}
		host = h
		if cls.Mode == ModeProxyAuthed {
			resolvedPlan = resolvePinnedProxyPlan(d.Resolver, PrincipalFrom(ctx), snap, modelName, h)
		}
	} else {
		if cls.Mode != ModeProxyAuthed {
			d.fireUsageFailure(ctx, "missing_upstream_host", "anonymous proxy traffic requires an upstream-host header")
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_host",
				"anonymous proxy traffic requires "+httpheader.HeaderUpstreamHost+" header naming a configured host")
			return
		}
		plan, err := resolveProxyHostByPolicy(d.Resolver, PrincipalFrom(ctx), snap, modelName)
		if err != nil {
			d.fireUsageFailure(ctx, "proxy_resolve_error", err.Error())
			mapProxyResolveErr(w, err)
			return
		}
		host = plan.Host
		resolvedPlan = plan
	}

	// Resolve system rate-limit bucket + subject for this mode.
	bucketName := systemRLProxyAuthed
	subject := relayKeyHashSubject(ctx)
	if cls.Mode == ModeProxyAnonymous {
		bucketName = systemRLProxyAnon
		subject = cls.ClientIP
	}
	rules := resolveSystemRules(snap, bucketName, subject)
	// With no policy there is no rule to select nor a policy slug to anchor
	// the hash tag on, so a policy-less key keeps the system bucket alone.
	var inbound *policy.InboundInput
	if p := PrincipalFrom(ctx); cls.Mode == ModeProxyAuthed && p != nil && p.Policy != nil {
		inbound = proxyInbound(ctx, p.Policy, resolvedPlan, host, rules)
		subject, rules = "", nil
	}

	// Strip relay-internal headers before forward — Authorization, X-WR-*,
	// hop-by-hop. Proxy.Run re-attaches the caller's UpstreamAuth.
	forwardHdr := r.Header.Clone()
	httpheader.Strip(forwardHdr)

	// Forward to the upstream's native path, not the inbound vendor-
	// namespaced path. Post-#195 inbound paths carry a /openai or
	// /anthropic prefix (e.g. /anthropic/v1/messages) that the upstream
	// does not expect; the spec's UpstreamPath is the un-prefixed target
	// (e.g. /v1/messages). Falls back to the raw path when no spec maps it.
	spec := d.Specs.Spec(adapterKind)
	upstreamPath := proxyUpstreamPath(r.URL.Path, spec, host)

	if lc != nil {
		if host != nil {
			lc.HostID = host.Meta.ID
			lc.HostName = host.Meta.Name
		}
		if p := PrincipalFrom(ctx); p != nil {
			lc.PolicyID = p.PolicyID()
		}
		applyPlanIdentity(lc, resolvedPlan) // Plan overrides when present
		if spec != nil {
			lc.Translator = spec.Translator
		}
	}
	preq := &proxy.Request{
		Method:        r.Method,
		Path:          upstreamPath,
		Body:          reqBody,
		ContentLength: r.ContentLength,
		Headers:       forwardHdr,
		HostBaseURL:   host.Spec.BaseURL,
		UpstreamAuth:  cls.UpstreamAuth,
		Inbound:       inbound,
		RateScope:     subject,
		Rules:         rules,
		Extractor:     extractorFor(d, adapterKind),
		Lifecycle:     lc,
	}

	slog.Debug("proxy: calling upstream",
		"request_id", reqid.From(ctx),
		"host", host.Meta.Name,
		"base_url", host.Spec.BaseURL,
		"inbound_path", r.URL.Path,
		"upstream_path", upstreamPath,
	)
	result, err := d.Proxy.Run(ctx, preq)
	if err != nil {
		slog.Warn("proxy: upstream call returned error", "request_id", reqid.From(ctx), "err", err)
		mapProxyErr(w, err)
		return
	}
	slog.Debug("proxy: upstream responded; streaming back",
		"request_id", reqid.From(ctx),
		"status", result.Status,
	)
	body := reqBody.wrapResult(result.Body, lc)
	defer body.Close()
	// An upstream that answers before reading the whole body answered an incomplete one; the refusal is the caller's answer.
	if rej := reqBody.rejection(); rej != nil {
		writeAPIError(w, rej.status, "invalid_request_error", rej.code, rej.msg)
		return
	}

	ForwardUpstreamHeaders(w.Header(), result.Headers)
	w.WriteHeader(result.Status)
	n, copyErr := streamCopy(w, body)
	slog.Debug("proxy: stream complete",
		"request_id", reqid.From(ctx),
		"bytes", n,
		"copy_err", copyErr,
	)
}

// proxyPinAllowed reports whether p may name h in the upstream-host header. A keyless host ignores the forwarded credential, so the caller's own key authorizes nothing there; as in normal routing, reaching one takes an explicit policy grant on that host.
func proxyPinAllowed(snap *appcatalog.Snapshot, p *Principal, h *apphost.Host) bool {
	if !h.Spec.NoAuth {
		return true
	}
	return p != nil && p.Policy != nil && p.Policy.IsEnabled() && snap.PolicyGrantsHost(p.Policy.Meta.ID, h.Meta.ID)
}

func proxyUpstreamPath(inboundPath string, spec *adapter.Spec, host *apphost.Host) string {
	// The host's own path wins verbatim (explicit "" = baseURL is the whole
	// endpoint), mirroring the pipeline flow's Host.Spec.Path semantics.
	if host != nil && host.Spec.Path != nil {
		return *host.Spec.Path
	}
	if spec != nil && spec.DefaultPath != "" {
		return spec.DefaultPath
	}
	return inboundPath
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
	rl, ok := snap.SystemRateLimitByName(bucketName)
	if !ok {
		return nil
	}
	return apprl.ResolveWithScope("proxy", subject, rl)
}

// proxyInbound is an authed proxy request's inbound reservation: pol's rate
// limit, selected from the routed plan as normal mode selects it (or from the
// pinned host alone when nothing routed), with ceiling reserved in the same
// call. ceiling's keys carry the key-hash subject, so sharing the policy's
// hash tag keeps them per key.
func proxyInbound(ctx context.Context, pol *policy.Policy, plan *routing.Plan, host *apphost.Host, ceiling []pkgratelimit.Rule) *policy.InboundInput {
	teamID, tokenJTI := reserveIdentity(ctx)
	in := &policy.InboundInput{Policy: pol, TeamID: teamID, TokenJTI: tokenJTI, ExtraRules: ceiling}
	switch {
	case plan != nil:
		in.ProviderSlug = plan.Provider
		if plan.Model != nil {
			in.ModelSlug, in.ModelID = plan.Model.Meta.Name, plan.Model.Meta.ID
		}
		if plan.Host != nil {
			in.HostSlug = plan.Host.Meta.Name
		}
	case host != nil:
		in.HostSlug = host.Meta.Name
	}
	return in
}

// resolvePinnedProxyPlan routes modelName at the pinned host, as normal mode
// folds the header pin, so a pinned request is metered and priced like a
// routed one. A miss is nil, not a refusal. A ref whose own "@" names another
// host is a miss too: the bytes still go to h.
func resolvePinnedProxyPlan(resolver *routing.Resolver, p *Principal, snap *appcatalog.Snapshot, modelName string, h *apphost.Host) *routing.Plan {
	if resolver == nil || p == nil || modelName == "" {
		return nil
	}
	ref := modelName
	if !strings.Contains(ref, "@") {
		ref += "@" + h.Meta.Name
	}
	plan, err := resolver.Resolve(routing.Request{
		ModelName:    ref,
		RawModelName: modelName,
		Policy:       p.Policy,
		UserID:       p.UserID,
		SkipKeyCheck: true,
		Snapshot:     snap,
	})
	if err != nil || plan.Host == nil || plan.Host.Meta.ID != h.Meta.ID {
		return nil
	}
	return plan
}

// relayKeyHashSubject returns the SHA-256 hash of the key token
// from ctx, suitable as a per-key rate-limit subject. Empty when no
// relay key is on ctx (proxy-anonymous path).
func relayKeyHashSubject(ctx context.Context) string {
	p := PrincipalFrom(ctx)
	if p == nil {
		return ""
	}
	// The principal already carries the hash it authenticated with; a token
	// has none, so its jti is the per-credential subject.
	if p.KeyHash != "" {
		return p.KeyHash
	}
	if p.CredentialKind == CredentialToken {
		return p.CredentialID
	}
	// Fallback shouldn't happen — KeyByHash matched by hash.
	cls := ClassificationFrom(ctx)
	return apikey.Hash(cls.Key)
}

func mapProxyErr(w http.ResponseWriter, err error) {
	var rej *bodyError
	switch {
	case errors.As(err, &rej):
		writeAPIError(w, rej.status, "invalid_request_error", rej.code, rej.msg)
	case errors.Is(err, proxy.ErrNoUpstreamAuth):
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_auth",
			"proxy mode requires Authorization: Bearer <upstream-key>")
	case errors.Is(err, pkgratelimit.ErrRevoked):
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "token_revoked",
			"this token has been revoked")
	case errors.Is(err, proxy.ErrPolicyMissing):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "policy_missing", "proxy mode is not wired to a policy service")
	case errors.Is(err, pkgratelimit.ErrUnavailable):
		writeStoreUnavailable(w)
	default:
		// Limiter exceeded → 429 with Retry-After from the limiter's
		// bucket-refill timing, so SDK clients back off correctly instead of
		// falling into their naked-429 hammer loop.
		var ex *pkgratelimit.KeyQuotaExhausted
		if errors.As(err, &ex) {
			// Same condition as the pipeline path, so the same envelope: two error.type values for one rejection makes it unclassifiable by any client that branches on it.
			secs := setRetryAfter(w, ex.RetryAfter)
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
				rateLimitMessage(ex, secs))
			return
		}
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", httpheader.SafeUpstreamError("upstream", err), "err", err)
	}
}

// strconvQuote wraps strconv.Quote without importing strconv at the
// top of every file in this package. Inline because the only use is in
// error messages.
func strconvQuote(s string) string { return "\"" + s + "\"" }

// proxyMaxBodyBuffer caps what proxy mode holds of a body while waiting
// for its model, and what payload capture keeps of a streamed one. 32 MiB
// matches the Caddy front-proxy request cap and the upstream providers'
// own request limits; a larger body without its model in the first 32 MiB
// is rejected.
const proxyMaxBodyBuffer = 32 << 20

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

// resolveProxyHostByPolicy routes model through the catalog resolver with
// SkipKeyCheck, so proxy mode reuses normal mode's policy + binding logic
// without requiring keypool coverage.
func resolveProxyHostByPolicy(resolver *routing.Resolver, p *Principal, snap *appcatalog.Snapshot, model string) (*routing.Plan, error) {
	if p == nil {
		return nil, &errProxyHostResolve{Reason: "relay_key_required", Detail: "policy-driven host resolution requires an authenticated relay key"}
	}
	return resolveProxyPlan(model, resolver, p, snap)
}

// scanReadBody reads body whole and scans it.
func scanReadBody(body io.Reader) (*scannedBody, *bodyError) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, sourceBodyError(err)
	}
	return scannedWhole(raw)
}

func resolveProxyPlan(model string, resolver *routing.Resolver, p *Principal, snap *appcatalog.Snapshot) (*routing.Plan, error) {
	plan, err := resolver.Resolve(routing.Request{
		ModelName:    model,
		Policy:       p.Policy,
		UserID:       p.UserID,
		SkipKeyCheck: true,
		Snapshot:     snap,
	})
	if err != nil {
		return nil, &errProxyHostResolve{Reason: "routing", Detail: "could not resolve host from policy + model", Model: model, Inner: err}
	}
	return plan, nil
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
