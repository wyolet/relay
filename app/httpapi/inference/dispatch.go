// Package inference's Dispatch is the shape-agnostic per-request flow.
// It owns: classification branching (proxy vs normal), routing resolution,
// translator chaining (inbound ↔ openai ↔ upstream), pipeline invocation,
// and response wrapping.
//
// Per-shape routes (under app/adapters/<name>/routes.go) own only:
//   1. Minimal parse to extract the model name + stream flag.
//   2. Translator selection (the route knows its own inbound Name).
//   3. The Dispatch call.
//
// This keeps shape-specific files out of app/httpapi/inference/.
package inference

import (
	"bufio"
	"bytes"
	"io"
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
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

// Dispatch runs the shape-agnostic flow. Called from per-shape route
// handlers (e.g. app/adapters/openai/routes.go) after they've done a
// minimal parse to extract ModelName + Stream.
func Dispatch(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput) {
	ctx := r.Context()

	// Proxy mode short-circuits cross-shape translation: BYO upstream
	// key paths through Proxy.Run with no body rewrite.
	cls := ClassificationFrom(ctx)
	if cls.Mode == ModeProxyAuthed || cls.Mode == ModeProxyAnonymous {
		// Reset r.Body so handleProxy can re-read it.
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

	// Phase 1: Responses inbound only supports OpenAI upstream. Ollama,
	// Bedrock-compat, Groq, and other OpenAI-shape hosts don't expose
	// /v1/responses; we'd forward to the wrong path with no value. Phase 2
	// adds cross-shape translation (canonical-at-center) and lifts this guard.
	if in.Inbound == adapters.OpenAIResponses {
		if plan.HostBinding.Adapter != adapters.OpenAI || plan.Host.Meta.Name != "openai" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "responses_unsupported_host",
				"model "+in.ModelName+" is on host "+plan.Host.Meta.Name+
					" which does not support the Responses API; use /openai/v1/chat/completions instead")
			return
		}
	}

	// Phase 1: Embeddings inbound requires an OpenAI-compatible upstream.
	// Anthropic hosts don't expose /v1/embeddings; any other OpenAI-shape
	// host (Voyage, Together, Fireworks, Cohere compat, Ollama, etc.) is
	// accepted. Phase 2 is N/A — Anthropic has no embeddings API to translate
	// to, so this guard is permanent.
	if in.Inbound == adapters.OpenAIEmbeddings {
		if plan.HostBinding.Adapter != adapters.OpenAI {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "embeddings_unsupported_host",
				"model "+in.ModelName+" is on host "+plan.Host.Meta.Name+
					" (adapter="+string(plan.HostBinding.Adapter)+") which does not support OpenAI-compatible embeddings")
			return
		}
	}

	// When the inbound shape is one of the alt-path OpenAI variants, use the
	// dedicated upstream adapter (POSTs to /v1/responses or /v1/embeddings)
	// and matching translator key. sameShape=true → byte-passthrough.
	upstreamKey := plan.HostBinding.Adapter
	if in.Inbound == adapters.OpenAIResponses {
		upstreamKey = adapters.OpenAIResponses
	}
	if in.Inbound == adapters.OpenAIEmbeddings {
		upstreamKey = adapters.OpenAIEmbeddings
	}

	upstreamAdapter, ok := d.Adapters[upstreamKey]
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(upstreamKey))
		return
	}

	inboundT := d.Translators.Get(in.Inbound)
	upstreamT := d.Translators.Get(upstreamKey)
	if inboundT == nil || upstreamT == nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_translator",
			"missing translator for "+string(in.Inbound)+" or "+string(upstreamKey))
		return
	}

	sameShape := in.Inbound == upstreamKey

	// Build the wire body for the upstream call.
	wireBody, err := buildWireBody(in.Body, plan.Snapshot.Upstream(), sameShape, inboundT, upstreamT)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", err.Error())
		return
	}

	preq := &pipeline.Request{
		Body:        wireBody,
		Headers:     r.Header,
		HostBaseURL: plan.Host.Spec.BaseURL,
		Adapter:     upstreamAdapter,
		Policy:      plan.Policy,
		Model:       plan.Model,
		Host:        plan.Host,
		Provider:    plan.Provider,
		Keys:        plan.Keys,
		ModelName:   plan.Model.Meta.Name,
	}

	result, err := d.Pipeline.Run(ctx, preq)
	if err != nil {
		mapPipelineErr(w, err)
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

	if sameShape {
		_, _ = io.Copy(w, result.Body)
		return
	}

	if in.Stream {
		streamTranslated(w, result.Body, upstreamT, inboundT)
		return
	}
	bufferTranslated(w, result.Body, upstreamT, inboundT)
}

// buildWireBody produces the request body that hits the upstream.
//
//   - same-shape: byte-equivalent passthrough; just rewrite the model
//     field to the snapshot's upstream name.
//   - cross-shape: parse inbound → openai → serialize upstream. The
//     model field on the canonical request is set to the snapshot's
//     upstream name before serialization.
func buildWireBody(body []byte, upstreamModel string, sameShape bool, inboundT, upstreamT adapters.Translator) ([]byte, error) {
	if sameShape {
		return rewriteModelField(body, upstreamModel), nil
	}
	canon, err := inboundT.ParseRequest(body)
	if err != nil {
		return nil, err
	}
	canon.Model = upstreamModel
	return upstreamT.SerializeRequest(canon)
}

// streamTranslated chains upstream→openai→inbound chunk transformers on
// an SSE response. Each chunk is parsed at SSE boundary, transformed,
// flushed to the client.
//
// Either transformer can be nil (identity from openai's side); we skip
// the corresponding stage in that case.
func streamTranslated(w http.ResponseWriter, body io.ReadCloser, upstreamT, inboundT adapters.Translator) {
	upstreamToOpenAI := upstreamT.NewToOpenAIStream()
	openAIToInbound := inboundT.NewFromOpenAIStream()

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(splitSSEChunks)

	for scanner.Scan() {
		chunk := scanner.Bytes()
		out := chunk
		if upstreamToOpenAI != nil {
			translated, err := upstreamToOpenAI(out)
			if err != nil {
				return
			}
			out = translated
		}
		if openAIToInbound != nil {
			translated, err := openAIToInbound(out)
			if err != nil {
				return
			}
			out = translated
		}
		if len(out) == 0 {
			continue
		}
		// Translator outputs (anthropic.sseBytes) already terminate with
		// \n\n. Identity passthrough strips terminators via the scanner.
		// Trim then re-add exactly one \n\n so we don't double up and
		// confuse strict SSE clients (cc, Anthropic SDK).
		out = bytes.TrimRight(out, "\n")
		_, _ = w.Write(out)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// bufferTranslated handles the sync (non-streaming) response path:
// collect, upstream→openai parse, openai→inbound serialize, write.
func bufferTranslated(w http.ResponseWriter, body io.ReadCloser, upstreamT, inboundT adapters.Translator) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return
	}
	canon, err := upstreamT.ParseResponse(raw)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	out, err := inboundT.SerializeResponse(canon)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	_, _ = w.Write(out)
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

