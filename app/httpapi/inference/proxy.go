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

	"github.com/wyolet/relay/app/adapter"
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

// handleProxy is the shared proxy-mode flow called by /openai/v1/chat/completions
// and /anthropic/v1/messages. adapterKind pins the token extractor (and is used in
// future per-host adapter-mismatch checks).
func handleProxy(d Deps, w http.ResponseWriter, r *http.Request, adapterKind adapters.Name) {
	ctx := r.Context()
	cls := ClassificationFrom(ctx)
	slog.Debug("proxy: handle entry",
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
	lc := lifecycle.FromContext(ctx)
	var (
		host         *apphost.Host
		resolvedPlan *routing.Plan
		pb           proxyBody
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
		plan, b, err := resolveProxyHostByPolicy(r, d.Resolver, RelayKeyFromContext(ctx), lc != nil && lc.PayloadLog)
		if err != nil {
			d.fireUsageFailure(ctx, "proxy_resolve_error", err.Error())
			mapProxyResolveErr(w, err)
			return
		}
		host = plan.Host
		resolvedPlan = plan
		pb = b
		// Re-attach what the resolver consumed — the full buffered body
		// when it fit, or the peeked prefix chained onto the live stream —
		// so the forwarder sends exactly the bytes the customer sent.
		r.Body = pb.Reader
		pb.seedLifecycle(lc)
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
		if rk := RelayKeyFromContext(ctx); rk != nil {
			lc.PolicyID = rk.Spec.PolicyID
		}
		applyPlanIdentity(lc, resolvedPlan) // Plan overrides when present
		if spec != nil {
			lc.Translator = spec.Translator
		}
	}
	preq := &proxy.Request{
		Method:        r.Method,
		Path:          upstreamPath,
		Body:          r.Body,
		ContentLength: r.ContentLength,
		Headers:       forwardHdr,
		HostBaseURL:   host.Spec.BaseURL,
		UpstreamAuth:  cls.UpstreamAuth,
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
	body := pb.wrapResult(result.Body, lc)
	defer body.Close()

	ForwardUpstreamHeaders(w.Header(), result.Headers)
	w.WriteHeader(result.Status)
	n, copyErr := streamCopy(w, body)
	slog.Debug("proxy: stream complete",
		"request_id", reqid.From(ctx),
		"bytes", n,
		"copy_err", copyErr,
	)
}

func proxyUpstreamPath(inboundPath string, spec *adapter.Spec, host *apphost.Host) string {
	upstreamPath := inboundPath
	if spec != nil && spec.UpstreamPath != "" {
		upstreamPath = spec.UpstreamPath
	}
	if host != nil && host.Spec.Backend != nil {
		if override := host.Spec.Backend["upstreamPath"]; override != "" {
			return override
		}
	}
	return upstreamPath
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

// proxyMaxBodyPeek is the prefix window resolveProxyHostByPolicy buffers
// when peeking the inbound JSON body for its `model` field. It is a peek,
// not a size gate: bodies that fit are parsed in full, larger ones are
// scanned for the top-level `model` and streamed upstream. Long-context
// requests routinely run several MiB.
const proxyMaxBodyPeek = 1 << 20

// proxyMaxBodyBuffer caps full buffering when `model` is not within the
// peek window (pathological key order forces a complete parse). 32 MiB
// matches the Caddy front-proxy request cap and the upstream providers'
// own request limits; anything larger is rejected.
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

// proxyBody is the body handoff from resolveProxyHostByPolicy: the reader
// to forward upstream, the buffered prefix retained for payload capture
// (a reference, never mutated), and whether that prefix is incomplete —
// i.e. the remainder streams from the live request body.
type proxyBody struct {
	Reader    io.ReadCloser
	Prefix    []byte
	Truncated bool
	// Capture is armed on the streamed path when payload logging wants
	// the full body: the remainder tees into it as it flows upstream.
	// wrapResult finalizes it into the lifecycle context on response
	// close. Nil when the body was fully buffered or logging is off.
	Capture *bodyCapture
}

// seedLifecycle retains the resolver's buffered bytes on lc as the
// payload-capture fallback. When a Capture is armed, finalize overwrites
// both fields with the assembled full body; until then (and on any
// failure path that never closes the response) the prefix + truncated
// flag stand. Nil-safe on lc.
func (pb proxyBody) seedLifecycle(lc *lifecycle.Context) {
	if lc == nil {
		return
	}
	lc.RequestBody = pb.Prefix
	lc.RequestBodyTruncated = pb.Truncated
}

// wrapResult arms the capture handoff: the returned body's Close
// publishes the assembled capture into lc before the inner Close spawns
// the detached post-flight goroutine, so observers never see a torn
// buffer. Pass-through when no capture is armed.
func (pb proxyBody) wrapResult(body io.ReadCloser, lc *lifecycle.Context) io.ReadCloser {
	if pb.Capture == nil || lc == nil {
		return body
	}
	return &finalizeReadCloser{inner: body, c: pb.Capture, lc: lc}
}

// resolveProxyHostByPolicy reads enough of r.Body to learn the top-level
// `model` field and runs the catalog routing resolver with SkipKeyCheck so
// proxy mode can reuse the same policy + binding logic as normal mode
// without requiring keypool coverage. Bodies within proxyMaxBodyPeek come
// back fully buffered; larger bodies whose model appears in the peek window
// are streamed (prefix + remaining r.Body) without further buffering; only
// when the model sits beyond the window does buffering continue, up to
// proxyMaxBodyBuffer.
//
// captureFull arms a tee on the streamed path so payload logging stores
// the whole body (capped at proxyMaxBodyBuffer) instead of the bare
// prefix — without serializing client upload and upstream send.
func resolveProxyHostByPolicy(r *http.Request, resolver *routing.Resolver, rk *relaykey.RelayKey, captureFull bool) (*routing.Plan, proxyBody, error) {
	if rk == nil {
		return nil, proxyBody{}, &errProxyHostResolve{Reason: "relay_key_required", Detail: "policy-driven host resolution requires an authenticated relay key"}
	}
	prefix, err := io.ReadAll(io.LimitReader(r.Body, proxyMaxBodyPeek+1))
	if err != nil {
		return nil, proxyBody{}, &errProxyHostResolve{Reason: "body_read", Detail: "could not read request body", Inner: err}
	}
	if len(prefix) <= proxyMaxBodyPeek {
		plan, err := resolveProxyModelJSON(prefix, resolver, rk)
		if err != nil {
			return nil, proxyBody{}, err
		}
		return plan, proxyBody{Reader: io.NopCloser(bytes.NewReader(prefix)), Prefix: prefix}, nil
	}

	if model, ok := scanTopLevelModel(prefix); ok {
		plan, err := resolveProxyPlan(model, resolver, rk)
		if err != nil {
			return nil, proxyBody{}, err
		}
		pb := proxyBody{Prefix: prefix, Truncated: true}
		rest := io.Reader(r.Body)
		if captureFull {
			pb.Capture = newBodyCapture(prefix, proxyMaxBodyBuffer)
			rest = pb.Capture.tee(r.Body)
		}
		pb.Reader = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix), rest), r.Body}
		return plan, pb, nil
	}

	rest, err := io.ReadAll(io.LimitReader(r.Body, int64(proxyMaxBodyBuffer-len(prefix))+1))
	if err != nil {
		return nil, proxyBody{}, &errProxyHostResolve{Reason: "body_read", Detail: "could not read request body", Inner: err}
	}
	full := append(prefix, rest...)
	if len(full) > proxyMaxBodyBuffer {
		return nil, proxyBody{}, &errProxyHostResolve{Reason: "body_too_large", Detail: "request body exceeds proxy buffer limit"}
	}
	plan, err := resolveProxyModelJSON(full, resolver, rk)
	if err != nil {
		return nil, proxyBody{}, err
	}
	return plan, proxyBody{Reader: io.NopCloser(bytes.NewReader(full)), Prefix: full}, nil
}

func resolveProxyModelJSON(body []byte, resolver *routing.Resolver, rk *relaykey.RelayKey) (*routing.Plan, error) {
	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return nil, &errProxyHostResolve{Reason: "body_not_json", Detail: "proxy mode without X-WR-Upstream-Host requires a JSON body carrying a 'model' field", Inner: err}
	}
	if peek.Model == "" {
		return nil, &errProxyHostResolve{Reason: "missing_model", Detail: "request body has no 'model' field; cannot resolve upstream host without it"}
	}
	return resolveProxyPlan(peek.Model, resolver, rk)
}

func resolveProxyPlan(model string, resolver *routing.Resolver, rk *relaykey.RelayKey) (*routing.Plan, error) {
	plan, err := resolver.Resolve(routing.Request{
		ModelName:    model,
		RelayKey:     rk,
		SkipKeyCheck: true,
	})
	if err != nil {
		return nil, &errProxyHostResolve{Reason: "routing", Detail: "could not resolve host from policy + model", Model: model, Inner: err}
	}
	return plan, nil
}

// scanTopLevelModel extracts the top-level "model" string from possibly
// truncated JSON. It token-walks the object and returns as soon as the
// value is decoded, so a decoder error past that point (the cut-off tail)
// never matters. Nested containers are consumed whole, which is what keeps
// a "model" key inside messages from matching.
func scanTopLevelModel(b []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, ok := keyTok.(json.Delim); ok && d == '}' {
			return "", false
		}
		key, _ := keyTok.(string)
		valTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if key == "model" {
			s, ok := valTok.(string)
			return s, ok && s != ""
		}
		if d, ok := valTok.(json.Delim); ok && (d == '{' || d == '[') {
			depth := 1
			for depth > 0 {
				t, err := dec.Token()
				if err != nil {
					return "", false
				}
				if dd, ok := t.(json.Delim); ok {
					switch dd {
					case '{', '[':
						depth++
					case '}', ']':
						depth--
					}
				}
			}
		}
	}
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
