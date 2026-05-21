// Cross-shape Responses dispatch: when Responses inbound hits a host that
// can't speak Responses natively. Two upstream shapes are supported:
//
//   - OpenAI-compatible (Adapter=OpenAI, host != "openai") — Ollama, Together,
//     Groq, Fireworks, Azure OpenAI, Bedrock OpenAI-compat. Translate
//     Responses ↔ Chat Completions via cctranslator and forward to
//     /v1/chat/completions.
//
//   - Anthropic (Adapter=Anthropic) — api.anthropic.com, Bedrock Claude,
//     Vertex Claude. Translate Responses ↔ /v1/messages via
//     anthropictranslator.
//
// OpenAI proper (Adapter=OpenAI, host="openai") never reaches this file —
// inference.Dispatch keeps that path as a byte-pass to /v1/responses.
//
// Responses-only fields (previous_response_id, store, background,
// conversation, include[], context_management, service_tier,
// safety_identifier, prompt_cache_key) are rejected by each translator's
// request stage and surface here as a 400.
//
// Why this bypasses the adapters.Translator interface
//
// The Translator interface in app/adapters/translator.go works against the
// OpenAI Chat Completions hub (canonical = openai.FullChatRequest /
// openai.ChatResponse). Responses can't round-trip through that hub without
// losing tool taxonomy, reasoning state, and the typed item array, so
// Responses inbound runs against direct pairwise translators (cctranslator,
// anthropictranslator) wired here. This is Phase 1.5 of docs/canonical-protocol.md;
// Phase 2 reorganizes both inbound paths against a relay-internal canonical
// and this file's hook goes away.

package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
	"github.com/wyolet/relay/pkg/adapters/openai/responses/anthropictranslator"
	"github.com/wyolet/relay/pkg/adapters/openai/responses/cctranslator"
)

// DispatchResponsesCrossShape is the inference.CrossShapeHandler for
// adapters.OpenAIResponses. Registered on inference.Deps.CrossShapeHandlers
// by the composition root.
func DispatchResponsesCrossShape(d inference.Deps, w http.ResponseWriter, r *http.Request, in inference.DispatchInput, plan *routing.Plan) {
	ctx := r.Context()

	req, err := responses.Parse(in.Body)
	if err != nil {
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_responses_request", err.Error())
		return
	}
	req.Model = plan.Snapshot.Upstream()

	upstreamKey := plan.HostBinding.Adapter
	upstreamAdapter, ok := d.Adapters[upstreamKey]
	if !ok {
		inference.WriteAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
			"no adapter registered for "+string(upstreamKey))
		return
	}

	var (
		wireBody    []byte
		toResponse  func([]byte) (*responses.Response, error)
		streamTrans responsesStreamTranslator
	)

	switch upstreamKey {
	case adapters.OpenAI:
		ccReq, terr := cctranslator.RequestToCC(req)
		if terr != nil {
			inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", terr.Error())
			return
		}
		wireBody, err = json.Marshal(ccReq)
		if err != nil {
			inference.WriteAPIError(w, http.StatusInternalServerError, "server_error", "marshal_request", err.Error())
			return
		}
		toResponse = func(b []byte) (*responses.Response, error) {
			var cc pkgopenai.ChatResponse
			if uerr := json.Unmarshal(b, &cc); uerr != nil {
				return nil, uerr
			}
			return cctranslator.CCToResponse(req, &cc, req.Model)
		}
		streamTrans = cctranslator.NewStream(req)

	case adapters.Anthropic:
		wireBody, err = anthropictranslator.RequestToAnthropic(req)
		if err != nil {
			inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", err.Error())
			return
		}
		toResponse = func(b []byte) (*responses.Response, error) {
			return anthropictranslator.AnthropicToResponse(req, b)
		}
		streamTrans = anthropictranslator.NewStream(req)

	default:
		inference.WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "responses_unsupported_host",
			"model "+in.ModelName+" is on host "+plan.Host.Meta.Name+
				" (adapter="+string(upstreamKey)+") which has no Responses-capable upstream")
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
		inference.MapPipelineErr(w, err)
		return
	}
	defer result.Body.Close()

	if in.Stream {
		// Streaming: upstream Content-Length is meaningless for the
		// translated SSE; Go will chunk-encode the response.
		inference.ForwardUpstreamHeaders(w.Header(), result.Headers)
		w.Header().Del("Content-Length")
		w.WriteHeader(result.Status)
		streamResponsesFrames(w, result.Body, streamTrans)
		return
	}

	// Sync: translate first so we can set the actual Content-Length on the
	// translated body (upstream's value describes a different byte size).
	out := translateBufferedBody(result.Body, toResponse)
	inference.ForwardUpstreamHeaders(w.Header(), result.Headers)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(result.Status)
	_, _ = w.Write(out)
}

// translateBufferedBody reads the full upstream body, runs the translator,
// and returns the bytes to write to the client. On translator or marshal
// failure the raw upstream body passes through.
func translateBufferedBody(body io.ReadCloser, toResponse func([]byte) (*responses.Response, error)) []byte {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil
	}
	resp, err := toResponse(raw)
	if err != nil {
		return raw
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return raw
	}
	return out
}

// responsesStreamTranslator is the common per-chunk interface satisfied by
// both cctranslator.Stream and anthropictranslator.Stream.
type responsesStreamTranslator interface {
	Translate(chunk []byte) ([]responses.SSEFrame, error)
}

// streamResponsesFrames consumes SSE chunks from body, feeds each to the
// stream translator, and emits the resulting Responses SSE frames. One
// upstream chunk often produces several Responses events (e.g. the first
// CC delta opens response.created, response.in_progress, output_item.added,
// content_part.added, output_text.delta — five frames).
func streamResponsesFrames(w http.ResponseWriter, body io.ReadCloser, st responsesStreamTranslator) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(inference.SplitSSEChunks)

	for scanner.Scan() {
		chunk := append([]byte(nil), scanner.Bytes()...)
		// Reattach the SSE separator the scanner stripped — translators
		// expect the raw frame they would receive on the wire.
		chunk = append(chunk, '\n', '\n')

		frames, err := st.Translate(chunk)
		if err != nil {
			return
		}
		for _, f := range frames {
			b := f.Bytes()
			if len(b) == 0 {
				continue
			}
			_, _ = w.Write(b)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}
