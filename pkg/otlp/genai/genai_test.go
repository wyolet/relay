package genai

import (
	"reflect"
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

// Audio tokens are a breakdown of the input and output totals, so the totals keep them; the image and text breakdowns are not carried.
func TestMapSpanAudioTokens(t *testing.T) {
	inf, ok := Mapper{}.MapSpan(span(otlp.Attrs{
		"gen_ai.operation.name":                "chat",
		"gen_ai.request.model":                 "example-model",
		"gen_ai.usage.input_tokens":            int64(1000),
		"gen_ai.usage.cache_read.input_tokens": int64(400),
		"gen_ai.usage.audio.input_tokens":      int64(250),
		"gen_ai.usage.image.input_tokens":      int64(90),
		"gen_ai.usage.text.input_tokens":       int64(660),
		"gen_ai.usage.output_tokens":           int64(300),
		"gen_ai.usage.audio.output_tokens":     int64(120),
		"gen_ai.usage.text.output_tokens":      int64(180),
	}))
	if !ok {
		t.Fatal("not mapped")
	}
	want := otlp.TokenCounts{Input: 600, Output: 300, CacheRead: 400, AudioInput: 250, AudioOutput: 120}
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

func event(attrs otlp.Attrs) otlp.LogRecord {
	return otlp.LogRecord{
		TraceID:   "5b8efff798038103d269b633813fc60c",
		SpanID:    "eee19b7ec3c1b174",
		EventName: "gen_ai.client.inference.operation.details",
		Time:      spanStart,
		Observed:  spanStart.Add(time.Second),
		Attrs:     attrs,
		Resource:  otlp.Attrs{"service.name": "billing-agent"},
	}
}

func TestMapLogReadsTheSameAttributesAsMapSpan(t *testing.T) {
	attrs := otlp.Attrs{
		"gen_ai.operation.name":                "chat",
		"gen_ai.provider.name":                 "example-provider",
		"gen_ai.request.model":                 "example-model",
		"gen_ai.response.id":                   "resp_123",
		"gen_ai.response.finish_reasons":       []any{"stop"},
		"gen_ai.usage.input_tokens":            int64(1000),
		"gen_ai.usage.cache_read.input_tokens": int64(700),
		"gen_ai.usage.output_tokens":           int64(300),
		"error.type":                           "timeout",
		"http.response.status_code":            int64(504),
	}
	fromEvent, ok := Mapper{}.MapLog(event(attrs))
	if !ok {
		t.Fatal("inference details event not mapped")
	}
	fromSpan, ok := Mapper{}.MapSpan(span(attrs))
	if !ok {
		t.Fatal("chat span not mapped")
	}
	// A log record has no end time; everything else about the call is the same record.
	if fromEvent.Duration != 0 {
		t.Errorf("event duration = %v, want 0", fromEvent.Duration)
	}
	fromSpan.Duration = 0
	if !reflect.DeepEqual(fromEvent, fromSpan) {
		t.Errorf("event maps to\n%+v\nspan maps to\n%+v", fromEvent, fromSpan)
	}
}

func TestMapLogEventName(t *testing.T) {
	attrs := otlp.Attrs{"gen_ai.operation.name": "chat", "gen_ai.request.model": "example-model"}

	// Exporters older than the event_name field carry the name as an attribute.
	older := event(otlp.Attrs{"gen_ai.operation.name": "chat", "gen_ai.request.model": "example-model", "event.name": "gen_ai.client.inference.operation.details"})
	older.EventName = ""
	if _, ok := (Mapper{}).MapLog(older); !ok {
		t.Error("event named by the event.name attribute not mapped")
	}

	for _, name := range []string{"", "gen_ai.evaluation.result", "gen_ai.user.message", "exception"} {
		r := event(attrs)
		r.EventName = name
		if _, ok := (Mapper{}).MapLog(r); ok {
			t.Errorf("log record with event name %q mapped as a model call", name)
		}
	}

	agent := event(otlp.Attrs{"gen_ai.operation.name": "invoke_agent", "gen_ai.usage.input_tokens": int64(10)})
	if _, ok := (Mapper{}).MapLog(agent); ok {
		t.Error("an event for an agent operation mapped as a model call")
	}
}

func TestMapLogTimeAndIdentity(t *testing.T) {
	attrs := otlp.Attrs{"gen_ai.operation.name": "chat", "gen_ai.request.model": "example-model", "gen_ai.response.id": "resp_123"}

	unstamped := event(attrs)
	unstamped.Time = time.Time{}
	if inf, _ := (Mapper{}).MapLog(unstamped); !inf.Start.Equal(spanStart.Add(time.Second)) {
		t.Errorf("start = %v, want the observed time when the event carries no time of its own", inf.Start)
	}

	outside := event(attrs)
	outside.TraceID, outside.SpanID = "", ""
	inf, ok := Mapper{}.MapLog(outside)
	if !ok || inf.TraceID != "" || inf.SpanID != "" || inf.ResponseID != "resp_123" {
		t.Errorf("event outside a span = %+v (%v), want no span ids and the response id", inf, ok)
	}
}

func TestContentIsPassedThroughAsReported(t *testing.T) {
	structured := []any{map[string]any{"role": "user", "parts": []any{map[string]any{"type": "text", "content": "hello"}}}}
	attrs := otlp.Attrs{
		"gen_ai.operation.name":      "chat",
		"gen_ai.request.model":       "example-model",
		"gen_ai.system_instructions": `[{"type":"text","content":"be brief"}]`,
		"gen_ai.input.messages":      structured,
		"gen_ai.tool.definitions":    "",
		"gen_ai.output.messages":     `[{"role":"assistant"}]`,
		// The earlier content forms are not read.
		"gen_ai.prompt":     "hello",
		"gen_ai.completion": "hi",
	}
	wantInput := map[string]any{
		"gen_ai.system_instructions": `[{"type":"text","content":"be brief"}]`,
		"gen_ai.input.messages":      structured,
	}
	wantOutput := map[string]any{"gen_ai.output.messages": `[{"role":"assistant"}]`}

	fromSpan, _ := Mapper{}.MapSpan(span(attrs))
	fromEvent, _ := Mapper{}.MapLog(event(attrs))
	for name, inf := range map[string]otlp.Inference{"span": fromSpan, "event": fromEvent} {
		if !reflect.DeepEqual(inf.Content.Input, wantInput) || !reflect.DeepEqual(inf.Content.Output, wantOutput) {
			t.Errorf("%s content = %+v", name, inf.Content)
		}
	}

	bare, _ := Mapper{}.MapSpan(span(otlp.Attrs{"gen_ai.operation.name": "chat", "gen_ai.request.model": "example-model"}))
	if !bare.Content.Empty() || bare.Content.Input != nil || bare.Content.Output != nil {
		t.Errorf("content of a call that reported none = %+v", bare.Content)
	}
}
