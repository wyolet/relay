// Package genai maps spans that follow the OpenTelemetry GenAI semantic conventions to otlp.Inference records.
//
// The conventions are still in development status and have renamed attributes between releases, so each field is read from its current name first and then from the names earlier releases used. Only spans for a model call are mapped: agent, workflow, tool, retrieval and memory spans are left alone, because an agent span repeats the token usage of the model calls beneath it. Message content, the log and metric signals, and provider-specific attributes are out of scope.
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

	attrErrorType  = []string{"error.type"}
	attrHTTPStatus = []string{"http.response.status_code"}
	attrService    = []string{"service.name"}
)

// errorTypeOther is the conventions' value for an error with no more specific type.
const errorTypeOther = "_OTHER"

// modelCallOperations are the gen_ai.operation.name values that are one request to a model.
var modelCallOperations = map[string]bool{
	"chat":             true,
	"generate_content": true,
	"text_completion":  true,
	"embeddings":       true,
}

// Mapper implements otlp.SpanMapper for the GenAI semantic conventions.
type Mapper struct{}

// Name implements otlp.SpanMapper.
func (Mapper) Name() string { return Name }

// MapSpan implements otlp.SpanMapper.
func (Mapper) MapSpan(s otlp.Span) (otlp.Inference, bool) {
	a := s.Attrs
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
		// Without an operation name (instrumentations older than the attribute), only a span that names a model and carries token usage is taken as a model call.
		return otlp.Inference{}, false
	}

	cacheRead, _ := a.Int(attrCacheRead...)
	cacheWrite, _ := a.Int(attrCacheWrite...)
	reasoning, _ := a.Int(attrReasoningTokens...)
	// The conventions count cached tokens inside input_tokens; some instrumentations report them beside it instead. A cached total above the input can only be the second form, and is left as reported.
	if cached := cacheRead + cacheWrite; input >= cached {
		input -= cached
	}

	inf := otlp.Inference{
		Convention:     Name,
		TraceID:        s.TraceID,
		SpanID:         s.SpanID,
		Service:        s.Resource.Str(attrService...),
		Start:          s.Start,
		Operation:      op,
		Provider:       a.Str(attrProvider...),
		RequestModel:   requestModel,
		ResponseModel:  responseModel,
		ResponseID:     a.Str(attrResponseID...),
		ConversationID: a.Str(attrConversation...),
		ErrorType:      a.Str(attrErrorType...),
		Tokens: otlp.TokenCounts{
			Input:      nonNegative(input),
			Output:     nonNegative(output),
			CacheRead:  nonNegative(cacheRead),
			CacheWrite: nonNegative(cacheWrite),
			Reasoning:  nonNegative(reasoning),
		},
	}
	if !s.Start.IsZero() && s.End.After(s.Start) {
		inf.Duration = s.End.Sub(s.Start)
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
	}
	if inf.ErrorType == "" && s.Failed {
		inf.ErrorType = errorTypeOther
	}
	return inf, true
}

func nonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
