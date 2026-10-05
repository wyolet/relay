package genai

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/otlp"
)

var spanStart = time.Date(2026, 8, 4, 10, 52, 22, 0, time.UTC)

func span(attrs otlp.Attrs) otlp.Span {
	return otlp.Span{
		TraceID:  "5b8efff798038103d269b633813fc60c",
		SpanID:   "eee19b7ec3c1b174",
		Start:    spanStart,
		End:      spanStart.Add(2 * time.Second),
		Attrs:    attrs,
		Resource: otlp.Attrs{"service.name": "billing-agent"},
	}
}

func TestMapSpanCurrentNames(t *testing.T) {
	inf, ok := Mapper{}.MapSpan(span(otlp.Attrs{
		"gen_ai.operation.name":                 "chat",
		"gen_ai.provider.name":                  "example-provider",
		"gen_ai.request.model":                  "example-model",
		"gen_ai.response.model":                 "example-model-2026-08-01",
		"gen_ai.response.id":                    "resp_123",
		"gen_ai.conversation.id":                "conv_9",
		"gen_ai.response.finish_reasons":        []any{"stop"},
		"gen_ai.request.stream":                 true,
		"gen_ai.response.time_to_first_chunk":   0.4,
		"gen_ai.usage.input_tokens":             int64(1000),
		"gen_ai.usage.cache_read.input_tokens":  int64(700),
		"gen_ai.usage.cache_write.input_tokens": int64(200),
		"gen_ai.usage.output_tokens":            int64(300),
		"gen_ai.usage.reasoning.output_tokens":  int64(120),
	}))
	if !ok {
		t.Fatal("chat span not mapped")
	}
	// input_tokens includes cached tokens under the conventions; the record keeps only the uncached part.
	want := otlp.TokenCounts{Input: 100, Output: 300, CacheRead: 700, CacheWrite: 200, Reasoning: 120}
	if inf.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", inf.Tokens, want)
	}
	if inf.Convention != Name || inf.Operation != "chat" || inf.Provider != "example-provider" {
		t.Errorf("identity = %+v", inf)
	}
	if inf.RequestModel != "example-model" || inf.ResponseModel != "example-model-2026-08-01" {
		t.Errorf("models = %q / %q", inf.RequestModel, inf.ResponseModel)
	}
	if inf.ResponseID != "resp_123" || inf.ConversationID != "conv_9" || inf.FinishReason != "stop" {
		t.Errorf("response fields = %+v", inf)
	}
	if !inf.Streamed || inf.TimeToFirstChunk != 400*time.Millisecond || inf.Duration != 2*time.Second {
		t.Errorf("timing = streamed %v first chunk %v duration %v", inf.Streamed, inf.TimeToFirstChunk, inf.Duration)
	}
	if inf.Service != "billing-agent" || !inf.Start.Equal(spanStart) || inf.TraceID == "" || inf.SpanID == "" {
		t.Errorf("origin = %+v", inf)
	}
	if inf.ErrorType != "" || inf.HTTPStatus != 0 {
		t.Errorf("unexpected error fields: %q %d", inf.ErrorType, inf.HTTPStatus)
	}
}

func TestMapSpanEarlierNames(t *testing.T) {
	inf, ok := Mapper{}.MapSpan(span(otlp.Attrs{
		"gen_ai.operation.name":                    "chat",
		"gen_ai.system":                            "example-provider",
		"gen_ai.request.model":                     "example-model",
		"gen_ai.usage.prompt_tokens":               int64(500),
		"gen_ai.usage.completion_tokens":           int64(80),
		"gen_ai.usage.cache_creation.input_tokens": int64(50),
	}))
	if !ok {
		t.Fatal("span with earlier attribute names not mapped")
	}
	want := otlp.TokenCounts{Input: 450, Output: 80, CacheWrite: 50}
	if inf.Tokens != want || inf.Provider != "example-provider" {
		t.Errorf("got %+v provider %q, want %+v", inf.Tokens, inf.Provider, want)
	}
}

// Some instrumentations report cached tokens beside input_tokens rather than inside it; subtracting would go negative, so the input is kept as reported.
func TestMapSpanCachedTokensBesideInput(t *testing.T) {
	inf, ok := Mapper{}.MapSpan(span(otlp.Attrs{
		"gen_ai.operation.name":                "chat",
		"gen_ai.request.model":                 "example-model",
		"gen_ai.usage.input_tokens":            int64(2),
		"gen_ai.usage.cache_read.input_tokens": int64(40803),
		"gen_ai.usage.output_tokens":           int64(425),
	}))
	if !ok {
		t.Fatal("not mapped")
	}
	want := otlp.TokenCounts{Input: 2, Output: 425, CacheRead: 40803}
	if inf.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", inf.Tokens, want)
	}
}

func TestMapSpanSkipsSpansThatAreNotModelCalls(t *testing.T) {
	usage := otlp.Attrs{"gen_ai.usage.input_tokens": int64(10), "gen_ai.usage.output_tokens": int64(5)}
	for _, op := range []string{"invoke_agent", "invoke_workflow", "execute_tool", "retrieval", "fetch_response", "create_agent", "plan"} {
		attrs := otlp.Attrs{"gen_ai.operation.name": op, "gen_ai.request.model": "example-model"}
		for k, v := range usage {
			attrs[k] = v
		}
		if _, ok := (Mapper{}).MapSpan(span(attrs)); ok {
			t.Errorf("operation %q mapped; its usage repeats the model calls beneath it", op)
		}
	}
	if _, ok := (Mapper{}).MapSpan(span(otlp.Attrs{"http.request.method": "POST"})); ok {
		t.Error("span without gen_ai attributes mapped")
	}
	if _, ok := (Mapper{}).MapSpan(span(otlp.Attrs{"gen_ai.request.model": "example-model"})); ok {
		t.Error("span with a model but no operation and no usage mapped")
	}
}

func TestMapSpanWithoutOperationName(t *testing.T) {
	inf, ok := Mapper{}.MapSpan(span(otlp.Attrs{
		"gen_ai.system":              "example-provider",
		"gen_ai.response.model":      "example-model",
		"gen_ai.usage.input_tokens":  "12",
		"gen_ai.usage.output_tokens": 3.0,
	}))
	if !ok {
		t.Fatal("span naming a model and its usage not mapped")
	}
	if inf.Tokens.Input != 12 || inf.Tokens.Output != 3 || inf.ResponseModel != "example-model" {
		t.Errorf("got %+v", inf)
	}
}

func TestMapSpanErrors(t *testing.T) {
	s := span(otlp.Attrs{
		"gen_ai.operation.name":     "chat",
		"gen_ai.request.model":      "example-model",
		"error.type":                "rate_limit_exceeded",
		"http.response.status_code": int64(429),
	})
	s.Failed = true
	inf, ok := Mapper{}.MapSpan(s)
	if !ok || inf.ErrorType != "rate_limit_exceeded" || inf.HTTPStatus != 429 {
		t.Errorf("got ok=%v %+v", ok, inf)
	}

	s = span(otlp.Attrs{"gen_ai.operation.name": "chat", "gen_ai.request.model": "example-model"})
	s.Failed = true
	if inf, _ := (Mapper{}).MapSpan(s); inf.ErrorType != "_OTHER" {
		t.Errorf("failed span without error.type: ErrorType = %q", inf.ErrorType)
	}
}

func TestMapSpanMissingTimes(t *testing.T) {
	s := span(otlp.Attrs{"gen_ai.operation.name": "embeddings", "gen_ai.request.model": "example-embedder", "gen_ai.usage.input_tokens": int64(9)})
	s.End = time.Time{}
	inf, ok := Mapper{}.MapSpan(s)
	if !ok || inf.Duration != 0 || inf.Tokens.Input != 9 {
		t.Errorf("got ok=%v %+v", ok, inf)
	}
}
