// Cross-shape Responses dispatch: when Responses inbound hits a host that
// can't speak Responses natively. Two upstream shapes are supported:
//
//   - OpenAI-compatible (Adapter=OpenAI, host != "openai") — Ollama, Together,
//     Groq, Fireworks, Azure OpenAI, Bedrock OpenAI-compat. Translate Responses
//     ↔ Chat Completions via cctranslator and forward to /v1/chat/completions.
//
//   - Anthropic (Adapter=Anthropic) — api.anthropic.com, Bedrock Claude,
//     Vertex Claude. Translate Responses ↔ /v1/messages via anthropictranslator.
//
// OpenAI proper (Adapter=OpenAI, host="openai") never reaches this file —
// dispatch.go keeps that path as a byte-pass to /v1/responses.
//
// Responses-only fields (previous_response_id, store, background, conversation,
// include[], context_management, service_tier, safety_identifier,
// prompt_cache_key) are rejected by each translator's request stage and
// surface here as a 400.

package inference

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
	"github.com/wyolet/relay/pkg/adapters/openai/responses/anthropictranslator"
	"github.com/wyolet/relay/pkg/adapters/openai/responses/cctranslator"
)

// dispatchResponsesCrossShape runs the Responses request through the
// translator pair that matches plan.HostBinding.Adapter. Caller has already
// resolved plan and verified that this is not the OpenAI-proper byte-pass case.
func dispatchResponsesCrossShape(d Deps, w http.ResponseWriter, r *http.Request, in DispatchInput, plan *routing.Plan) {
	ctx := r.Context()

	req, err := responses.Parse(in.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_responses_request", err.Error())
		return
	}
	req.Model = plan.Snapshot.Upstream()

	upstreamKey := plan.HostBinding.Adapter
	upstreamAdapter, ok := d.Adapters[upstreamKey]
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter",
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
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", terr.Error())
			return
		}
		wireBody, err = json.Marshal(ccReq)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "server_error", "marshal_request", err.Error())
			return
		}
		modelOverride := req.Model
		toResponse = func(b []byte) (*responses.Response, error) {
			var cc pkgopenai.ChatResponse
			if uerr := json.Unmarshal(b, &cc); uerr != nil {
				return nil, uerr
			}
			return cctranslator.CCToResponse(&cc, modelOverride)
		}
		streamTrans = ccStreamAdapter{s: cctranslator.NewStream()}

	case adapters.Anthropic:
		wireBody, err = anthropictranslator.RequestToAnthropic(req)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "translate_request", err.Error())
			return
		}
		toResponse = anthropictranslator.AnthropicToResponse
		streamTrans = anthropicStreamAdapter{s: anthropictranslator.NewStream()}

	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "responses_unsupported_host",
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

	if in.Stream {
		streamResponsesFrames(w, result.Body, streamTrans)
		return
	}
	bufferResponses(w, result.Body, toResponse)
}

// bufferResponses reads the full upstream body, translates to a Responses
// response struct, marshals to JSON, and writes. On translator failure the
// raw upstream body is passed through (matches bufferTranslated's behavior).
func bufferResponses(w http.ResponseWriter, body io.ReadCloser, toResponse func([]byte) (*responses.Response, error)) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return
	}
	resp, err := toResponse(raw)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	out, err := json.Marshal(resp)
	if err != nil {
		_, _ = w.Write(raw)
		return
	}
	_, _ = w.Write(out)
}

// responsesStreamTranslator is the common Translate-per-chunk interface
// satisfied by both cctranslator.Stream and anthropictranslator.Stream.
// Their SSEFrame types differ but both expose Event/Data and Bytes(); we
// adapt each through a small wrapper below.
type responsesStreamTranslator interface {
	Translate(chunk []byte) ([][]byte, error)
}

type ccStreamAdapter struct{ s *cctranslator.Stream }

func (c ccStreamAdapter) Translate(chunk []byte) ([][]byte, error) {
	frames, err := c.s.Translate(chunk)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Bytes())
	}
	return out, nil
}

type anthropicStreamAdapter struct{ s *anthropictranslator.Stream }

func (a anthropicStreamAdapter) Translate(chunk []byte) ([][]byte, error) {
	frames, err := a.s.Translate(chunk)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Bytes())
	}
	return out, nil
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
	scanner.Split(splitSSEChunks)

	for scanner.Scan() {
		chunk := append([]byte(nil), scanner.Bytes()...)
		// Reattach the SSE separator the scanner stripped — translators
		// expect the raw frame they would receive on the wire.
		chunk = append(chunk, '\n', '\n')

		out, err := st.Translate(chunk)
		if err != nil {
			return
		}
		for _, b := range out {
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
