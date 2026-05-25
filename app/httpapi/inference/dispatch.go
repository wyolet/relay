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
	"io"
	"log/slog"
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	v1 "github.com/wyolet/relay/pkg/relay/v1"
	"github.com/wyolet/relay/pkg/reqid"
)

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

	// Proxy mode short-circuits cross-shape translation.
	cls := ClassificationFrom(ctx)
	slog.Info("inference: dispatch entry",
		"request_id", reqid.From(ctx),
		"inbound", string(in.Inbound),
		"model", in.ModelName,
		"stream", in.Stream,
		"mode", cls.Mode,
		"body_bytes", len(in.Body),
	)
	if cls.Mode == ModeProxyAuthed || cls.Mode == ModeProxyAnonymous {
		r.Body = io.NopCloser(bytes.NewReader(in.Body))
		handleProxy(d, w, r, in.Inbound)
		return
	}

	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "missing relay key")
		return
	}

	plan, err := d.Resolver.Resolve(routing.Request{
		ModelName: in.ModelName,
		RelayKey:  rk,
	})
	if err != nil {
		mapRoutingErr(w, err)
		return
	}

	inboundSpec := d.Specs.Spec(in.Inbound)
	if inboundSpec == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_spec",
			"no adapter spec registered for "+string(in.Inbound))
		return
	}

	// Byte-pass shapes (e.g. Embeddings) skip all translation and use the
	// inbound spec's own adapter regardless of upstream binding.
	if inboundSpec.BytePass {
		upstreamAdapter := d.Specs.PipelineAdapter(in.Inbound)
		if upstreamAdapter == nil {
			writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
				"no adapter registered for "+string(in.Inbound))
			return
		}
		// Embeddings inbound requires an OpenAI-compatible upstream adapter.
		// Anthropic hosts don't expose /v1/embeddings. This guard is permanent
		// (Anthropic has no embeddings API to translate to).
		if plan.HostBinding.Adapter != adapters.OpenAI {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "embeddings_unsupported_host",
				"model "+in.ModelName+" is on host "+plan.Host.Meta.Name+
					" (adapter="+string(plan.HostBinding.Adapter)+") which does not support OpenAI-compatible embeddings")
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
	upstreamSpec := d.Specs.Spec(plan.HostBinding.Adapter)
	if upstreamSpec == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_spec",
			"no adapter spec registered for upstream "+string(plan.HostBinding.Adapter))
		return
	}
	upstreamAdapter := d.Specs.PipelineAdapter(plan.HostBinding.Adapter)
	if upstreamAdapter == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(plan.HostBinding.Adapter))
		return
	}

	sameShape := in.Inbound == plan.HostBinding.Adapter

	if sameShape {
		runBytePass(d, w, r, in, plan, upstreamAdapter, upstreamSpec.Translator)
		return
	}

	// Cross-shape: both sides must have canonical translators.
	inboundV1 := inboundSpec.Translator
	upstreamV1 := upstreamSpec.Translator
	if inboundV1 == nil || upstreamV1 == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_translator",
			"missing canonical translator for "+string(in.Inbound)+" or "+string(plan.HostBinding.Adapter))
		return
	}

	dispatchCanonical(d, w, r, in, plan, upstreamAdapter, inboundV1, upstreamV1)
}

// runBytePass handles same-shape or byte-pass dispatch: forward the body
// (with model field rewritten to the upstream model name) to the upstream
// and stream or buffer the response back.
func runBytePass(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput, plan *routing.Plan, upstreamAdapter pipeline.Adapter, upstreamV1 v1.Translator) {
	ctx := r.Context()

	wireBody := rewriteModelField(in.Body, plan.Snapshot.Upstream())

	cls := ClassificationFrom(ctx)
	lc := buildLifecycleContext(ctx, "pipeline", cls.RelayKey, plan)
	lc.Translator = upstreamV1
	preq := &pipeline.Request{
		Body:          wireBody,
		Headers:       r.Header,
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       upstreamAdapter,
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.Snapshot.Upstream(),
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
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", err.Error())
		return
	}
	canonReq.Model = v1.ModelRefs{plan.Snapshot.Upstream()}
	applyOutputDefaults(canonReq, plan.Model.Spec.MaxOutputTokens)

	wireBody, err := upstreamV1.SerializeRequest(canonReq)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "marshal_request", err.Error())
		return
	}

	cls := ClassificationFrom(ctx)
	lc := buildLifecycleContext(ctx, "pipeline", cls.RelayKey, plan)
	lc.Translator = upstreamV1
	preq := &pipeline.Request{
		Body:          wireBody,
		Headers:       r.Header,
		HostBaseURL:   plan.Host.Spec.BaseURL,
		Adapter:       upstreamAdapter,
		Policy:        plan.Policy,
		Model:         plan.Model,
		Host:          plan.Host,
		Provider:      plan.Provider,
		Keys:          plan.Keys,
		ModelName:     plan.Model.Meta.Name,
		UpstreamModel: plan.Snapshot.Upstream(),
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
	w.WriteHeader(result.Status)

	if in.Stream {
		streamCanonical(w, result.Body, upstreamV1.NewToCanonicalStream(), inboundV1.NewFromCanonicalStream())
		return
	}
	bufferCanonical(w, result.Body, canonReq, upstreamV1, inboundV1)
}

// streamCanonical chains upstream→canonical→inbound per-chunk transforms.
// toCanon converts upstream SSE chunks to canonical SSE.
// fromCanon converts canonical SSE chunks to inbound SSE.
func streamCanonical(w http.ResponseWriter, body io.ReadCloser, toCanon, fromCanon func([]byte) ([]byte, error)) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(splitSSEChunks)

	for scanner.Scan() {
		chunk := append([]byte(nil), scanner.Bytes()...)
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
		_, _ = w.Write(out)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// bufferCanonical handles the sync (non-streaming) canonical cross-shape response.
func bufferCanonical(w http.ResponseWriter, body io.ReadCloser, canonReq *v1.Request, upstreamV1, inboundV1 v1.Translator) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return
	}
	canResp, err := upstreamV1.ParseResponse(raw)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	out, err := inboundV1.SerializeResponse(canResp, canonReq)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	_, _ = w.Write(out)
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
