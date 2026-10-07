// Package genai maps spans and events that follow the OpenTelemetry GenAI semantic conventions to otlp.Inference records.
//
// The conventions are still in development status and have renamed attributes between releases, so each field is read from its current name first and then from the names earlier releases used. Only records for a model call are mapped: the inference span and the inference details event. Agent, workflow, tool, retrieval and memory spans are left alone, because an agent span repeats the token usage of the model calls beneath it. Message content is passed through as reported. Beside the conventions' attributes, relay's own wyolet.relay.host is read: the catalog host relay's SDK resolved the call to. The metrics signal, the per-message events of earlier releases, and provider-specific attributes are out of scope.
package genai

import (
	"time"

	"github.com/wyolet/relay/pkg/otlp"
)

// Name identifies this convention on the records it produces.
const Name = "gen_ai"

// Attribute names, current first. The trailing names come from earlier releases of the conventions and from instrumentations that predate the dotted token names.
var (
	attrOperation     = []string{"gen_ai.operation.name"}
	attrProvider      = []string{"gen_ai.provider.name", "gen_ai.system"}
	attrRequestModel  = []string{"gen_ai.request.model"}
	attrResponseModel = []string{"gen_ai.response.model"}
	attrResponseID    = []string{"gen_ai.response.id"}
	attrConversation  = []string{"gen_ai.conversation.id"}
	attrFinishReasons = []string{"gen_ai.response.finish_reasons"}
	attrStream        = []string{"gen_ai.request.stream"}
	attrFirstChunk    = []string{"gen_ai.response.time_to_first_chunk"}

	attrInputTokens     = []string{"gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens"}
	attrOutputTokens    = []string{"gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens"}
	attrCacheRead       = []string{"gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_read_input_tokens"}
	attrCacheWrite      = []string{"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_creation_input_tokens"}
	attrReasoningTokens = []string{"gen_ai.usage.reasoning.output_tokens"}
	attrAudioInput      = []string{"gen_ai.usage.audio.input_tokens"}
	attrAudioOutput     = []string{"gen_ai.usage.audio.output_tokens"}

	// attrRelayHost is relay's own attribute, not a convention one: the SDK sets it to the catalog host a call was resolved to.
	attrRelayHost = []string{"wyolet.relay.host"}

	attrErrorType  = []string{"error.type"}
	attrHTTPStatus = []string{"http.response.status_code"}
	attrService    = []string{"service.name"}
	attrEventName  = []string{"event.name"}
)

// Content attributes, present only when the client opted in to capturing content. The forms earlier releases used (gen_ai.prompt, gen_ai.completion, one event per message) are not read.
var (
	contentInput  = []string{"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.tool.definitions"}
	contentOutput = []string{"gen_ai.output.messages"}
)

// eventInferenceDetails is the event that carries what the inference span carries, for clients that report model calls as events.
const eventInferenceDetails = "gen_ai.client.inference.operation.details"

// errorTypeOther is the conventions' value for an error with no more specific type.
const errorTypeOther = "_OTHER"

// modelCallOperations are the gen_ai.operation.name values that are one request to a model.
var modelCallOperations = map[string]bool{
	"chat":             true,
	"generate_content": true,
	"text_completion":  true,
	"embeddings":       true,
}

// Mapper implements otlp.SpanMapper and otlp.LogMapper for the GenAI semantic conventions.
type Mapper struct{}

// Name identifies the convention.
func (Mapper) Name() string { return Name }

// MapSpan implements otlp.SpanMapper.
func (Mapper) MapSpan(s otlp.Span) (otlp.Inference, bool) {
	inf, ok := inference(s.Attrs, s.Resource)
	if !ok {
		return otlp.Inference{}, false
	}
	inf.TraceID, inf.SpanID, inf.Start = s.TraceID, s.SpanID, s.Start
	if !s.Start.IsZero() && s.End.After(s.Start) {
		inf.Duration = s.End.Sub(s.Start)
	}
	if inf.ErrorType == "" && s.Failed {
		inf.ErrorType = errorTypeOther
	}
	return inf, true
}

// MapLog implements otlp.LogMapper. Only the inference details event is a model call; a log record has no duration, so the call's stays zero.
func (Mapper) MapLog(r otlp.LogRecord) (otlp.Inference, bool) {
	name := r.EventName
	if name == "" {
		name = r.Attrs.Str(attrEventName...)
	}
	if name != eventInferenceDetails {
		return otlp.Inference{}, false
	}
	inf, ok := inference(r.Attrs, r.Resource)
	if !ok {
		return otlp.Inference{}, false
	}
	inf.TraceID, inf.SpanID, inf.Start = r.TraceID, r.SpanID, r.Time
	if inf.Start.IsZero() {
		inf.Start = r.Observed
	}
	return inf, true
}

// inference reads the attributes the inference span and the inference details event share. It reports false when they do not describe one request to a model.
func inference(a, resource otlp.Attrs) (otlp.Inference, bool) {
	op := a.Str(attrOperation...)
	requestModel := a.Str(attrRequestModel...)
	responseModel := a.Str(attrResponseModel...)
	input, hasInput := a.Int(attrInputTokens...)
	output, hasOutput := a.Int(attrOutputTokens...)

	switch {
	case op != "":
		if !modelCallOperations[op] {
			return otlp.Inference{}, false
		}
	case (requestModel == "" && responseModel == "") || (!hasInput && !hasOutput):
		// Without an operation name (instrumentations older than the attribute), only a record that names a model and carries token usage is taken as a model call.
		return otlp.Inference{}, false
	}

	cacheRead, _ := a.Int(attrCacheRead...)
	cacheWrite, _ := a.Int(attrCacheWrite...)
	reasoning, _ := a.Int(attrReasoningTokens...)
	// Audio counts are a breakdown of input_tokens and output_tokens, so neither total is reduced by them. The image and text breakdowns have no counterpart on the record and stay inside the totals.
	audioInput, _ := a.Int(attrAudioInput...)
	audioOutput, _ := a.Int(attrAudioOutput...)
	// The conventions count cached tokens inside input_tokens; some instrumentations report them beside it instead. A cached total above the input can only be the second form, and is left as reported.
	if cached := cacheRead + cacheWrite; input >= cached {
		input -= cached
	}

	inf := otlp.Inference{
		Convention:     Name,
		Service:        resource.Str(attrService...),
		Operation:      op,
		Provider:       a.Str(attrProvider...),
		RequestModel:   requestModel,
		ResponseModel:  responseModel,
		ResponseID:     a.Str(attrResponseID...),
		ConversationID: a.Str(attrConversation...),
		Host:           a.Str(attrRelayHost...),
		ErrorType:      a.Str(attrErrorType...),
		Tokens: otlp.TokenCounts{
			Input:       nonNegative(input),
			Output:      nonNegative(output),
			CacheRead:   nonNegative(cacheRead),
			CacheWrite:  nonNegative(cacheWrite),
			Reasoning:   nonNegative(reasoning),
			AudioInput:  nonNegative(audioInput),
			AudioOutput: nonNegative(audioOutput),
		},
		Content: otlp.Content{
			Input:  present(a, contentInput),
			Output: present(a, contentOutput),
		},
	}
	if reasons := a.Strings(attrFinishReasons...); len(reasons) > 0 {
		inf.FinishReason = reasons[0]
	}
	if streamed, ok := a.Bool(attrStream...); ok {
		inf.Streamed = streamed
	}
	if seconds, ok := a.Float(attrFirstChunk...); ok && seconds > 0 {
		inf.TimeToFirstChunk = time.Duration(seconds * float64(time.Second))
		inf.Streamed = true
	}
	if status, ok := a.Int(attrHTTPStatus...); ok && status >= 100 && status <= 599 {
		inf.HTTPStatus = int(status)
	} else if status, ok := a.Int(attrErrorType...); ok && status >= 400 && status <= 599 {
		// Some instrumentations report the provider's HTTP status as the error type and carry no status attribute.
		inf.HTTPStatus = int(status)
	}
	return inf, true
}

// present returns the attributes among names that carry a value, by name, or nil when none does.
func present(a otlp.Attrs, names []string) map[string]any {
	var out map[string]any
	for _, name := range names {
		v, ok := a[name]
		if !ok || v == nil || v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(names))
		}
		out[name] = v
	}
	return out
}

func nonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
