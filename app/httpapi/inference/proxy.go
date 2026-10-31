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
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/proxy"
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
			writeBodyError(w, rej)
			return
		}
	}
	markConnection(w, reqBody)

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
	principal := PrincipalFrom(ctx)
	var (
		host     *apphost.Host
		resolved *plan.Unkeyed
	)
	if cls.UpstreamHost != "" {
		h, ok := snap.HostByName(cls.UpstreamHost)
		kind := "unknown_upstream_host"
		if ok && !proxyPinAllowed(principal, h) {
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
		if cls.Mode == ModeProxyAuthed && principal != nil {
			// Folded into the ref as normal mode folds the header pin, so a pinned request reaches only a model the caller's grants allow there and is priced like a routed one.
			ref := modelName
			if !strings.Contains(ref, "@") {
				ref += "@" + h.Meta.Name
			}
			resolved = principal.Caller.Routes.ResolveUnkeyed(ref)
			if resolved.Refused == nil && resolved.Host.Meta.ID != h.Meta.ID {
				d.fireUsageFailure(ctx, "upstream_host_conflict", "model names a host other than the upstream-host header")
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "upstream_host_conflict",
					"model "+strconvQuote(modelName)+" names a host other than X-WR-Upstream-Host")
				return
			}
		}
	} else {
		if cls.Mode != ModeProxyAuthed || principal == nil {
			d.fireUsageFailure(ctx, "missing_upstream_host", "anonymous proxy traffic requires an upstream-host header")
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_host",
				"anonymous proxy traffic requires X-WR-Upstream-Host header naming a configured host")
			return
		}
		resolved = principal.Caller.Routes.ResolveUnkeyed(modelName)
		if resolved.Refused == nil {
			host = resolved.Host
		}
	}
	if resolved != nil && resolved.Refused != nil {
		d.fireUsageFailure(ctx, routeRefusalKind(resolved.Refused), resolved.Refused.Error())
		mapRouteRefusal(w, resolved.Refused, modelName)
		return
	}

	// The system bucket for this mode, per credential (authed) or per client IP (anonymous), on top of an authed caller's own counters.
	bucketName := systemRLProxyAuthed
	subject := relayKeyHashSubject(ctx)
	if cls.Mode == ModeProxyAnonymous {
		bucketName = systemRLProxyAnon
		subject = cls.ClientIP
	}
	counters := systemCounters(snap, bucketName, subject)
	if cls.Mode == ModeProxyAuthed && principal != nil && len(principal.Caller.Counters) > 0 {
		counters = append(append(make([]limits.Counter, 0, len(principal.Caller.Counters)+len(counters)), principal.Caller.Counters...), counters...)
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

	var pricing *pricing.Pricing
	if lc != nil {
		lc.HostID = host.Meta.ID
		lc.HostName = host.Meta.Name
		if pol := principal.Policy(); pol != nil {
			lc.PolicyID, lc.PolicyName = pol.Meta.ID, pol.Meta.Name
		}
		if resolved != nil {
			lc.ModelID, lc.ModelName, lc.ProviderName = resolved.Model.Meta.ID, resolved.Model.Meta.Name, resolved.Provider
			if resolved.Pricing != nil {
				lc.PricingID, lc.PricingName = resolved.Pricing.Meta.ID, resolved.Pricing.Meta.Name
			}
			if resolved.ResolvedVia != "" {
				lc.Metadata["resolved_via"] = resolved.ResolvedVia
			}
		}
		if spec != nil {
			lc.Translator = spec.Translator
		}
	}
	if resolved != nil {
		pricing = resolved.Pricing
	}
	preq := &proxy.Request{
		Method:        r.Method,
		Path:          upstreamPath,
		Body:          reqBody,
		ContentLength: r.ContentLength,
		Headers:       forwardHdr,
		HostBaseURL:   host.Spec.BaseURL,
		UpstreamAuth:  cls.UpstreamAuth,
		Counters:      counters,
		Pricing:       pricing,
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
	fwdCtx, cancelFwd := context.WithCancel(ctx)
	defer cancelFwd()
	watch := watchForward(reqBody, d.ProxyBodyIdle, cancelFwd)
	defer watch.stop()
	result, err := d.Proxy.Run(fwdCtx, preq)
	// The body may still be streaming upstream when this answer goes out (an upstream that answers early); unless it was read to its end the connection closes after the answer.
	markConnection(w, reqBody)
	if err != nil {
		slog.Warn("proxy: upstream call returned error", "request_id", reqid.From(ctx), "err", err)
		mapProxyErr(w, spec, err)
		return
	}
	slog.Debug("proxy: upstream responded; streaming back",
		"request_id", reqid.From(ctx),
		"status", result.Status,
	)
	body := reqBody.wrapResult(result.Body, lc)
	defer body.Close()
	// Deferred after Close, so it runs before it: the watch has stopped, and recorded any stall, before post-flight reads the refusal.
	defer watch.stop()
	// A refusal found by now, a stall included, replaces the upstream's answer; one found later ends the stream. Post-flight records either and settles the reservation as cancelled (proxy.RefusingBody).
	if !watch.track(result.Abort) || reqBody.rejection() != nil {
		writeBodyError(w, reqBody.rejection())
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
	watch.stop()
	// The status is out, so a refusal can only reach the caller as a broken response, never as a clean success.
	if reqBody.Refusal() != nil {
		panic(http.ErrAbortHandler)
	}
}

// proxyPinAllowed reports whether p may name h in the upstream-host header. A keyless host ignores the forwarded credential, so the caller's own key authorizes nothing there; as in normal routing, reaching one takes a pool at that host linked by a caller policy.
func proxyPinAllowed(p *Principal, h *apphost.Host) bool {
	if !h.Spec.NoAuth {
		return true
	}
	return p != nil && p.Caller != nil && p.Caller.Refused == nil && p.Caller.ReachesHost(h.Meta.ID)
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

// systemCounters are the rules of the named system rate limit counted for subject; none without a subject or an enabled limit.
func systemCounters(snap *appcatalog.Snapshot, bucketName, subject string) []limits.Counter {
	if subject == "" {
		return nil
	}
	rl, ok := snap.SystemRateLimitByName(bucketName)
	if !ok || !rl.IsEnabled() {
		return nil
	}
	out := make([]limits.Counter, 0, len(rl.Spec.Rules))
	for _, r := range rl.Spec.Rules {
		out = append(out, limits.SystemRule(rl, r, subject))
	}
	return out
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

func mapProxyErr(w http.ResponseWriter, inbound *adapter.Spec, err error) {
	var rej *bodyError
	switch {
	case errors.As(err, &rej):
		writeBodyError(w, rej)
	case errors.Is(err, proxy.ErrNoUpstreamAuth):
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "missing_upstream_auth",
			"proxy mode requires Authorization: Bearer <upstream-key>")
	case errors.Is(err, proxy.ErrLimiterMissing):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "limiter_missing", "proxy mode is not wired to a limiter")
	case errors.Is(err, pkgratelimit.ErrUnavailable):
		writeOverloaded(w, overloadedFor(inbound))
	default:
		// A refused counter is a 429 with Retry-After from the limiter, in the same envelope as the pipeline path: two error.type values for one rejection makes it unclassifiable by any client that branches on it.
		var refused *limits.Refused
		if errors.As(err, &refused) {
			secs := setRetryAfter(w, refused.RetryAfter)
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", callerLimitCode(refused),
				rateLimitMessage(callerLimitMessage(refused), secs), "counter", refused.Counter.ID)
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

// scanReadBody reads body whole and scans it.
func scanReadBody(body io.Reader) (*scannedBody, *bodyError) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, sourceBodyError(err)
	}
	return scannedWhole(raw)
}
