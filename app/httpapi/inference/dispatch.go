// Package inference's Dispatch is the shape-agnostic per-request flow.
// It owns: classification branching (proxy vs normal), routing resolution,
// translator chaining (inbound ↔ canonical ↔ upstream), pipeline invocation,
// and response wrapping.
//
// Per-shape routes (registered via app/adapter.MountRoutes) own only:
//  1. Minimal parse to extract the model name + stream flag.
//  2. The Dispatch call with the inbound shape Name.
//
// This keeps shape-specific files out of app/httpapi/inference/.
package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/reqid"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// forwardHeaders returns a copy of the inbound headers safe to send to the
// upstream provider: relay-internal control headers and the relay credential
// (Authorization / X-Api-Key / X-WR-* / Cookie, per httpheader.StripDenylist)
// are removed so they never leak to the provider — the adapter injects the
// real upstream credential afterward. Cloned so the original request headers
// stay intact for logging / post-flight. Mirrors the proxy path's strip.
func forwardHeaders(h http.Header) http.Header {
	out := httpheader.Strip(h.Clone())
	// The relay negotiates its own upstream content-coding; the caller does
	// not get to dictate it. Dropping Accept-Encoding lets Go's transport add
	// gzip transparently and hand us a DECOMPRESSED body (Content-Encoding
	// stripped) — without this, a forwarded "Accept-Encoding: gzip" leaves the
	// upstream body gzipped, which the canonical translate path can't parse and
	// whose stale Content-Encoding header breaks the caller's gunzip.
	out.Del("Accept-Encoding")
	return out
}

// DispatchInput is what a per-shape route passes to Dispatch after its
// own minimal parse. The route knows the inbound shape Name (because it
// owns the URL); Dispatch handles everything from routing onward.
type DispatchInput struct {
	// Inbound is the wire shape the caller spoke (the route's Name).
	Inbound adapters.Name

	// Body is the raw inbound request body, already consumed from r.Body.
	Body []byte

	// ModelName is the caller-supplied model identifier extracted by the
	// per-shape minimal parse. Routing resolution uses this.
	ModelName string

	// Stream is the caller-supplied stream flag from the minimal parse.
	// Determines whether the response leg streams chunks or buffers + emits.
	Stream bool
}

// Dispatch runs the shape-agnostic flow. Called from route handlers after a
// minimal parse to extract ModelName + Stream.
func Dispatch(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput) {
	ctx := r.Context()

	// Mint the per-request lifecycle Context here, at the inference entry,
	// before routing — so routing failures (and proxy gating) can still
	// fire a usage event, and every downstream phase enriches this one
	// Context rather than minting its own. Routing fills the identity ids
	// later via applyPlanIdentity.
	cls := ClassificationFrom(ctx)
	lc := mintLifecycle(ctx, sourceForMode(cls.Mode), cls.RelayKey, cls.ClientIP)
	lc.RequestedModel = in.ModelName
	applyObsHeaders(lc, r.Header, d.TrustEventTime)
	// Retain the inbound body for the payloadlog observer (a reference, not
	// a copy — in.Body is already the fully-buffered request). The capture
	// gate (lc.PayloadLog) is set once routing resolves the opt-in.
	lc.RequestBody = in.Body
	ctx = lifecycle.ContextWith(ctx, lc)
	r = r.WithContext(ctx)

	// Run the pre-flight phase (today: the inflight-gauge observer). Every
	// path past this point must end in a Finalize — success and runner
	// failures fire it themselves; pre-runner rejections go through
	// fireUsageFailure — so pre-flight and post-flight stay paired.
	if d.Lifecycle != nil {
		if err := d.Lifecycle.RunPreFlight(ctx, lc, &lifecycle.PreFlightEvent{}); err != nil {
			d.fireUsageFailure(ctx, "pre_flight_aborted", err.Error())
			writeAPIError(w, http.StatusInternalServerError, "server_error", "pre_flight_aborted", err.Error())
			return
		}
	}

	slog.Debug("inference: dispatch entry",
		"request_id", reqid.From(ctx),
		"inbound", string(in.Inbound),
		"model", in.ModelName,
		"stream", in.Stream,
		"mode", cls.Mode,
		"body_bytes", len(in.Body),
	)
	if cls.Mode == ModeProxyAuthed || cls.Mode == ModeProxyAnonymous {
		// Proxy bypasses routing.Resolve; the only opt-in surface is the
		// authenticating relay key (anonymous proxy has none).
		if rk := RelayKeyFromContext(ctx); rk != nil {
			lc.PayloadLog = rk.Spec.PayloadLoggingEnabled
		}
		r.Body = io.NopCloser(bytes.NewReader(in.Body))
		handleProxy(d, w, r, in.Inbound)
		return
	}

	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		d.fireUsageFailure(ctx, "unauthenticated", "missing relay key")
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	// Fold the X-WR-Upstream-Host header into the model ref as a host pin,
	// unless the ref already pins one with "@host" (explicit pin wins).
	modelRef := in.ModelName
	if uh := r.Header.Get(httpheader.HeaderUpstreamHost); uh != "" && !strings.Contains(modelRef, "@") {
		modelRef = modelRef + "@" + uh
	}

	plan, err := d.Resolver.Resolve(routing.Request{
		ModelName:    modelRef,
		RawModelName: in.ModelName,
		RelayKey:     rk,
	})
	if err != nil {
		d.fireUsageFailure(ctx, routingErrKind(err), err.Error())
		mapRoutingErr(w, err, modelRef, rk.Spec.PolicyID)
		return
	}
	lc.PayloadLog = plan.PayloadLoggingEnabled

	inboundSpec := d.Specs.Spec(in.Inbound)
	if inboundSpec == nil {
		d.fireUsageFailure(ctx, "no_spec", "no adapter spec registered for "+string(in.Inbound))
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_spec",
			"no adapter spec registered for "+string(in.Inbound))
		return
	}

	// Byte-pass shapes (e.g. Embeddings) skip all translation and use the
	// inbound spec's own adapter regardless of upstream binding.
	if inboundSpec.BytePass {
		upstreamAdapter := d.Specs.PipelineAdapter(in.Inbound)
		if upstreamAdapter == nil {
			d.fireUsageFailure(ctx, "no_adapter", "no adapter registered for "+string(in.Inbound))
			writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
				"no adapter registered for "+string(in.Inbound))
			return
		}
		// Embeddings inbound requires an OpenAI-compatible upstream adapter.
		// Anthropic hosts don't expose /v1/embeddings. This guard is permanent
		// (Anthropic has no embeddings API to translate to).
		if plan.HostBinding.Spec.Adapter != adapters.OpenAI {
			d.fireUsageFailure(ctx, "embeddings_unsupported_host", "host does not support OpenAI-compatible embeddings")
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "embeddings_unsupported_host",
				"model "+in.ModelName+" is on host "+plan.Host.Meta.Name+
					" (adapter="+string(plan.HostBinding.Spec.Adapter)+") which does not support OpenAI-compatible embeddings")
			return
		}
		runBytePass(d, w, r, in, plan, upstreamAdapter, inboundSpec.Translator)
		return
	}

	// Native-path check: does the resolved host natively speak this inbound
	// shape? If so, byte-pass using the inbound spec's own adapter (which
	// posts to the correct upstream path, e.g. /v1/responses).
	if inboundSpec.IsNativePath != nil && inboundSpec.IsNativePath(plan) {
		upstreamAdapter := d.Specs.PipelineAdapter(in.Inbound)
		if upstreamAdapter == nil {
			d.fireUsageFailure(ctx, "no_adapter", "no adapter registered for "+string(in.Inbound))
			writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
				"no adapter registered for "+string(in.Inbound))
			return
		}
		runBytePass(d, w, r, in, plan, upstreamAdapter, inboundSpec.Translator)
		return
	}

	// Standard dispatch: look up the upstream spec and determine whether
	// this is a same-shape pass (inbound == upstream adapter) or a cross-
	// shape canonical translation.
	upstreamSpec := d.Specs.Spec(plan.HostBinding.Spec.Adapter)
	if upstreamSpec == nil {
		d.fireUsageFailure(ctx, "no_spec", "no adapter spec registered for upstream "+string(plan.HostBinding.Spec.Adapter))
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_spec",
			"no adapter spec registered for upstream "+string(plan.HostBinding.Spec.Adapter))
		return
	}
	upstreamAdapter := d.Specs.PipelineAdapter(plan.HostBinding.Spec.Adapter)
	if upstreamAdapter == nil {
		d.fireUsageFailure(ctx, "no_adapter", "no adapter registered for "+string(plan.HostBinding.Spec.Adapter))
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(plan.HostBinding.Spec.Adapter))
		return
	}

	sameShape := in.Inbound == plan.HostBinding.Spec.Adapter

	if sameShape {
		runBytePass(d, w, r, in, plan, upstreamAdapter, upstreamSpec.Translator)
		return
	}

	// Cross-shape: both sides must have canonical translators.
	inboundV1 := inboundSpec.Translator
	upstreamV1 := upstreamSpec.Translator
	if inboundV1 == nil || upstreamV1 == nil {
		d.fireUsageFailure(ctx, "no_translator", "missing canonical translator for "+string(in.Inbound)+" or "+string(plan.HostBinding.Spec.Adapter))
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_translator",
			"missing canonical translator for "+string(in.Inbound)+" or "+string(plan.HostBinding.Spec.Adapter))
		return
	}

	dispatchCanonical(d, w, r, in, plan, upstreamAdapter, inboundV1, upstreamV1)
}

// runBytePass handles same-shape or byte-pass dispatch: forward the body
// (with model field rewritten to the upstream model name) to the upstream
// and stream or buffer the response back.
func runBytePass(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput, plan *routing.Plan, upstreamAdapter pipeline.Adapter, upstreamV1 v1.Translator) {
	ctx := r.Context()

	wireBody := rewriteModelField(in.Body, plan.UpstreamModel())

	lc := lifecycle.FromContext(ctx)
	applyPlanIdentity(lc, plan)
	if lc != nil {
		lc.Translator = upstreamV1
	}
	preq := &pipeline.Request{
		Body:          wireBody,
		Headers:       forwardHeaders(r.Header),
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       upstreamAdapter,
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.UpstreamModel(),
		Stream:        in.Stream,
		Lifecycle:     lc,
	}

	result, err := d.Pipeline.Run(ctx, preq)
	if err != nil {
		mapPipelineErr(w, err)
		return
	}
	defer result.Body.Close()

	ForwardUpstreamHeaders(w.Header(), result.Headers)
	w.WriteHeader(result.Status)
	// Byte-pass is same-shape / vendor-native — relay_usage is canonical-only,
	// so nothing is injected here; the upstream body streams through verbatim.
	_, _ = streamCopy(w, result.Body)
}

// dispatchCanonical handles cross-shape dispatch via the canonical v1 chain.
// inboundV1 parses the inbound body and serializes the response back to the
// inbound wire shape. upstreamV1 serializes the canonical request and parses
// the upstream response.
// applyOutputDefaults seeds the canonical request's max-output-tokens from the
// catalog model when the caller left it unset. Vendor-neutral and rule-4 clean:
// Anthropic *requires* max_tokens on the wire (its adapter otherwise falls back
// to a fixed 4096 that silently caps high-output models), and for OpenAI/Gemini
// the published model max is a harmless ceiling the model can't exceed anyway.
// No-op when the catalog has no max or the caller already set one.
func applyOutputDefaults(req *v1.Request, modelMaxOutput int) {
	if modelMaxOutput <= 0 || len(req.ModelConfig) > 1 {
		return
	}
	opts := effectiveModelOpts(req)
	if opts.Sampling == nil {
		opts.Sampling = &v1.SamplingParams{}
	}
	if opts.Sampling.MaxTokens == nil {
		m := modelMaxOutput
		opts.Sampling.MaxTokens = &m
	}
}

// effectiveModelOpts returns the *ModelOpts a vendor SerializeRequest will
// resolve for req, mirroring the resolution every translator uses (entry keyed
// by the model, else the sole entry). Creates one keyed by the model if none
// exists. Caller guarantees len(ModelConfig) <= 1.
func effectiveModelOpts(req *v1.Request) *v1.ModelOpts {
	key := ""
	if len(req.Model) > 0 {
		key = req.Model[0]
	}
	if req.ModelConfig == nil {
		req.ModelConfig = map[string]*v1.ModelOpts{}
	}
	if o, ok := req.ModelConfig[key]; ok {
		return o
	}
	for _, o := range req.ModelConfig { // sole entry (key may differ from upstream name)
		return o
	}
	o := &v1.ModelOpts{}
	req.ModelConfig[key] = o
	return o
}

func dispatchCanonical(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput, plan *routing.Plan, upstreamAdapter pipeline.Adapter, inboundV1, upstreamV1 v1.Translator) {
	ctx := r.Context()

	canonReq, err := inboundV1.ParseRequest(in.Body)
	if err != nil {
		d.fireUsageFailure(ctx, "translate_request", err.Error())
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", err.Error())
		return
	}
	canonReq.Model = v1.ModelRefs{plan.UpstreamModel()}
	applyOutputDefaults(canonReq, plan.Model.Spec.MaxOutputTokens)

	wireBody, err := upstreamV1.SerializeRequest(canonReq)
	if err != nil {
		d.fireUsageFailure(ctx, "marshal_request", err.Error())
		writeAPIError(w, http.StatusInternalServerError, "server_error", "marshal_request", err.Error())
		return
	}

	lc := lifecycle.FromContext(ctx)
	applyPlanIdentity(lc, plan)
	if lc != nil {
		lc.Translator = upstreamV1
	}
	preq := &pipeline.Request{
		Body:          wireBody,
		Headers:       forwardHeaders(r.Header),
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       upstreamAdapter,
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.UpstreamModel(),
		Stream:        in.Stream,
		Lifecycle:     lc,
	}

	result, pErr := d.Pipeline.Run(ctx, preq)
	if pErr != nil {
		mapPipelineErr(w, pErr)
		return
	}
	defer result.Body.Close()

	ForwardUpstreamHeaders(w.Header(), result.Headers)

	// Upstream rejected pre-first-byte (4xx/5xx). Forward its error body
	// verbatim with the upstream status, mirroring the byte-pass path. The
	// provider's message (e.g. "...use /v1/responses instead") is the
	// actionable signal; running the success-shaped ParseResponse/SSE-scan on
	// an error body discards it and emits a generic translate failure or an
	// empty body — the bug this guards against. Errors are exempt from the
	// canonical-only output rule: the diagnostic beats the shape, and this is
	// the same uniform handling stream and buffered both need (no mid-stream
	// failover concern since no bytes have flowed yet).
	if result.Status >= 400 {
		forwardUpstreamError(w, result.Body, result.Status)
		return
	}
	// The body is re-serialized by the canonical chain (and may be
	// usage-injected), so any upstream content coding/length no longer
	// describes what we emit. Drop them — leaving Content-Encoding: gzip on a
	// plain translated body makes the caller's gunzip fail ("invalid header").
	// Byte-pass forwards the encoded body verbatim and keeps these (runBytePass).
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	// relay_usage echo is canonical-only: a vendor-shaped response (openai/
	// anthropic) never carries it — those callers get a clean vendor body.
	// Only a canonical caller (/v1/*, adapters.Canonical) gets relay's usage,
	// on the typed field (buffered) or the terminal event (stream).
	echo := usageEchoRequested(r) && in.Inbound == adapters.Canonical
	// Reasoning timing is stamped from canonical events, so it's only
	// available when the caller speaks canonical (input shape canonical,
	// any upstream); vendor-inbound and non-stream get no reasoning span.
	trackReasoning := in.Inbound == adapters.Canonical
	if in.Stream {
		w.WriteHeader(result.Status)
		streamCanonical(d, w, r, result.Body, echo, trackReasoning, upstreamV1.NewToCanonicalStream(), inboundV1.NewFromCanonicalStream())
		return
	}
	bufferCanonical(d, w, r, result.Body, result.Status, echo, canonReq, upstreamV1, inboundV1)
}

// streamCanonical chains upstream→canonical→inbound per-chunk transforms.
// toCanon converts upstream SSE chunks to canonical SSE.
// fromCanon converts canonical SSE chunks to inbound SSE.
//
// When echo is set (canonical caller + X-WR-Usage: full), it drives a
// lifecycle StreamSession over the raw upstream frames and, at end-of-stream,
// splices relay_usage into the terminal frame (the canonical
// generation.completed event) — never as a standalone frame, so the canonical
// client reads it off the event it already parses. One-frame lookahead lets
// us reach "the last frame" before flushing it.
func streamCanonical(d Deps, w http.ResponseWriter, r *http.Request, body io.ReadCloser, echo, trackReasoning bool, toCanon, fromCanon func([]byte) ([]byte, error)) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(splitSSEChunks)

	var sess *lifecycle.StreamSession
	var lc *lifecycle.Context
	if (echo || trackReasoning) && d.Lifecycle != nil {
		lc = lifecycle.FromContext(r.Context())
	}
	if echo && lc != nil {
		sess = d.Lifecycle.NewStreamSession(lc)
	}

	writeFrame := func(f []byte) {
		_, _ = w.Write(f)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	var held []byte // one-frame lookahead so the terminal frame can carry relay_usage
	for scanner.Scan() {
		chunk := append([]byte(nil), scanner.Bytes()...)
		sess.Observe(chunk) // nil-safe; raw upstream frame (for ExtractSummary)
		chunk = append(chunk, '\n', '\n')

		var out []byte
		if toCanon != nil {
			translated, err := toCanon(chunk)
			if err != nil {
				return
			}
			out = translated
		} else {
			out = chunk
		}

		// out is canonical here (toCanon ran). Time the reasoning span off
		// the canonical reasoning events the adapter emitted — start on the
		// first reasoning frame, end on the last.
		if trackReasoning && lc != nil && toCanon != nil {
			for _, f := range splitCanonFrames(out) {
				if v1.IsReasoningFrame(f) {
					lc.MarkReasoningStart()
					lc.MarkReasoningEnd()
				}
			}
		}

		if fromCanon != nil && len(out) > 0 {
			frames := splitCanonFrames(out)
			out = nil
			for _, f := range frames {
				translated, err := fromCanon(f)
				if err != nil {
					return
				}
				out = append(out, translated...)
			}
		}

		if len(out) == 0 {
			continue
		}
		out = bytes.TrimRight(out, "\n")
		if sess != nil {
			if held != nil {
				writeFrame(held)
			}
			held = out
			continue
		}
		writeFrame(out)
	}

	if sess == nil {
		return
	}
	lc.MarkEnd()
	sess.Finish()
	if held != nil {
		if ru := usagelog.EchoFromContext(lc); ru != nil {
			held = injectRelayUsageSSE(held, ru)
		}
		writeFrame(held)
	}
}

// forwardUpstreamError relays an upstream error response (4xx/5xx) to the
// caller verbatim. The upstream Content-Type was already copied by
// ForwardUpstreamHeaders; the stale upstream Content-Length/Encoding are
// dropped so the write reflects the bytes we actually emit. If the upstream
// sent no body (or it can't be read), a structured relay error envelope is
// synthesized so the caller never receives a bodiless status — the exact
// failure this guards against.
func forwardUpstreamError(w http.ResponseWriter, body io.ReadCloser, status int) {
	raw, err := io.ReadAll(body)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		writeAPIError(w, status, "upstream_error", "upstream_error",
			fmt.Sprintf("upstream returned status %d with no body", status))
		return
	}
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// bufferCanonical handles the sync (non-streaming) canonical cross-shape
// response. It owns the status write so it can drop the upstream
// Content-Length (the translated — and optionally usage-injected — body
// differs in length) and optionally inject the relay_usage echo block.
// Upstream non-2xx responses never reach here — dispatchCanonical forwards
// them verbatim via forwardUpstreamError — so status is always a success.
func bufferCanonical(d Deps, w http.ResponseWriter, r *http.Request, body io.ReadCloser, status int, echo bool, canonReq *v1.Request, upstreamV1, inboundV1 v1.Translator) {
	ctx := r.Context()
	raw, err := io.ReadAll(body)
	if err != nil {
		d.fireUsageFailure(ctx, "read_response", err.Error())
		writeAPIError(w, http.StatusBadGateway, "upstream_error", "read_response", err.Error())
		return
	}

	// Canonical contract: this caller asked for canonical and must receive
	// canonical — never the raw upstream vendor body. A parse/serialize
	// failure on a SUCCESS body is surfaced as a canonical error, not masked
	// by forwarding vendor bytes (which would leak vendor-shaped usage/output a
	// canonical client can't decode). See codebase rule 11.
	canResp, perr := upstreamV1.ParseResponse(raw)
	if perr != nil {
		d.fireUsageFailure(ctx, "translate_response", perr.Error())
		writeAPIError(w, http.StatusBadGateway, "upstream_error", "translate_response", perr.Error())
		return
	}
	if echo {
		setRelayUsage(d, r, status, raw, canResp)
	}
	// Canonical inbound serializes via the identity translator, so a set
	// canResp.RelayUsage lands as the typed relay_usage field. echo is
	// only ever true for canonical callers (see Dispatch).
	out, serr := inboundV1.SerializeResponse(canResp, canonReq)
	if serr != nil {
		d.fireUsageFailure(ctx, "serialize_response", serr.Error())
		writeAPIError(w, http.StatusInternalServerError, "server_error", "serialize_response", serr.Error())
		return
	}

	w.Header().Del("Content-Length") // body length changed by translation
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// usageEchoRequested reports whether the caller opted into the inline
// relay_usage echo block via X-WR-Usage: full.
func usageEchoRequested(r *http.Request) bool {
	return r.Header.Get(httpheader.HeaderUsage) == httpheader.UsageValueFull
}

// setRelayUsage fills the lifecycle hooks pre-send (idempotent — the
// post-flight Finalize won't refill) and sets the collected usage blob on
// the canonical response as the typed relay_usage field. No-op when no
// lifecycle is wired or no usage was collected. raw is the untranslated
// upstream body, used for token extraction.
func setRelayUsage(d Deps, r *http.Request, status int, raw []byte, canResp *v1.Response) {
	if d.Lifecycle == nil || canResp == nil {
		return
	}
	lc := lifecycle.FromContext(r.Context())
	if lc == nil {
		return
	}
	lc.MarkEnd()
	d.Lifecycle.Fill(lc, &lifecycle.PostFlightEvent{Status: status, ResponseBody: raw})
	if ru := usagelog.EchoFromContext(lc); ru != nil {
		canResp.RelayUsage = ru
	}
}

// injectRelayUsageSSE splices relay_usage into the JSON of an SSE frame's
// `data:` line (the canonical generation.completed terminal event). No-op
// when the frame has no JSON-object data line.
func injectRelayUsageSSE(frame []byte, ru *v1.RelayUsage) []byte {
	const marker = "data: "
	idx := bytes.Index(frame, []byte(marker))
	if idx < 0 {
		return frame
	}
	start := idx + len(marker)
	dataJSON := frame[start:]
	var tail []byte
	if nl := bytes.IndexByte(dataJSON, '\n'); nl >= 0 {
		dataJSON, tail = dataJSON[:nl], dataJSON[nl:]
	}
	spliced := injectRelayUsage(dataJSON, ru)
	out := make([]byte, 0, start+len(spliced)+len(tail))
	out = append(out, frame[:start]...)
	out = append(out, spliced...)
	out = append(out, tail...)
	return out
}

// injectRelayUsage splices a top-level "relay_usage" key into a JSON object
// body, preserving key order and avoiding a full re-marshal. Returns body
// unchanged when it isn't a JSON object.
func injectRelayUsage(body []byte, ru *v1.RelayUsage) []byte {
	val, err := json.Marshal(ru)
	if err != nil {
		return body
	}
	t := bytes.TrimRight(body, " \t\r\n")
	if len(t) == 0 || t[len(t)-1] != '}' {
		return body
	}
	prefix := bytes.TrimRight(t[:len(t)-1], " \t\r\n")
	var b bytes.Buffer
	b.Grow(len(prefix) + len(val) + 16)
	b.Write(prefix)
	if len(prefix) > 0 && prefix[len(prefix)-1] != '{' {
		b.WriteByte(',')
	}
	b.WriteString(`"relay_usage":`)
	b.Write(val)
	b.WriteByte('}')
	return b.Bytes()
}

// splitCanonFrames splits concatenated canonical SSE bytes into individual frames.
func splitCanonFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		idx := bytes.Index(b, []byte("\n\n"))
		if idx < 0 {
			trimmed := bytes.TrimSpace(b)
			if len(trimmed) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		frame := b[:idx+2]
		if len(bytes.TrimSpace(b[:idx])) > 0 {
			frames = append(frames, frame)
		}
		b = b[idx+2:]
	}
	return frames
}

// splitSSEChunks is a bufio.SplitFunc that splits on the SSE event
// terminator "\n\n". Returned tokens omit the terminator.
func splitSSEChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
