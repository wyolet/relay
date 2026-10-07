package telemetry

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var callStart = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

func goldenCall() Call {
	req := &v1.Request{
		Instructions: "Answer briefly.",
		Input: []v1.Item{
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{
				&v1.TextPart{Text: "Weather in Paris?"},
				&v1.ImagePart{ImageURL: "data:image/png;base64,iVBORw0KGgo="},
			}},
			&v1.Reasoning{Summary: []v1.SummaryText{{Text: "Need the tool."}}, ProviderData: json.RawMessage(`"sig"`)},
			&v1.FunctionCall{CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris","days":2}`},
			&v1.FunctionCallOutput{CallID: "call_1", Output: "rainy, 14°C"},
		},
		Tools: &v1.ToolsConfig{Definitions: v1.Tools{
			&v1.FunctionTool{Name: "get_weather", Description: "Current weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		}},
	}
	content := NewContent(req)
	content.SetOutput([]v1.Item{
		&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "Rainy, 14°C."}}},
	}, v1.FinishReasonStop)
	return Call{
		Provider:         "openai",
		Host:             "openai",
		ServerAddress:    "api.openai.com",
		RequestModel:     "gpt-4o",
		ResponseModel:    "gpt-4o-2024-08-06",
		ResponseID:       "chatcmpl-1",
		FinishReasons:    []string{"stop"},
		Stream:           true,
		TimeToFirstChunk: 250 * time.Millisecond,
		Start:            callStart,
		End:              callStart.Add(2 * time.Second),
		StatusCode:       200,
		ConversationID:   "conv_1",
		Usage:            usage.Tokens{"input": 176, "cache_read": 1024, "output": 300, "reasoning": 128, "audio_input": 40, "audio_output": 16, "accepted_prediction": 3},
		Parent:           "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		Content:          content,
	}
}

// goldenRecord is goldenCall's record with a fixed span id, so the encoding is reproducible.
func goldenRecord(t *testing.T) *record {
	t.Helper()
	c := goldenCall()
	r := newRecord(&c)
	if r.traceID != "0af7651916cd43dd8448eb211c80319c" || r.parentID != "b7ad6b7169203331" {
		t.Fatalf("ids = %s / parent %s, want the traceparent's", r.traceID, r.parentID)
	}
	r.spanID = "00f067aa0ba902b7"
	return r
}

func checkGolden(t *testing.T, name string, payload any) {
	t.Helper()
	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("%s differs from the golden file; run with -update to inspect:\n%s", name, got)
	}
}

func TestEncodeSpanGolden(t *testing.T) {
	r := goldenRecord(t)
	e := &Emitter{resource: resource{Attributes: []keyValue{stringAttr("service.name", "billing")}}, scope: scope{Name: sdkModule + "/telemetry"}}
	checkGolden(t, "traces.json", exportTraces{ResourceSpans: []resourceSpans{{Resource: e.resource, ScopeSpans: []scopeSpans{{Scope: e.scope, Spans: []span{r.span()}}}}}})
}

func TestEncodeEventGolden(t *testing.T) {
	r := goldenRecord(t)
	e := &Emitter{resource: resource{Attributes: []keyValue{stringAttr("service.name", "billing")}}, scope: scope{Name: sdkModule + "/telemetry"}}
	checkGolden(t, "logs.json", exportLogs{ResourceLogs: []resourceLogs{{Resource: e.resource, ScopeLogs: []scopeLogs{{Scope: e.scope, LogRecords: []logRecord{r.event()}}}}}})
}

func TestUsageAttributesIncludeCacheInInput(t *testing.T) {
	attrs := appendUsage(nil, usage.Tokens{"input": 90, "cache_read": 2000, "cache_creation": 500, "output": 210, "server_tool_use_input": 7})
	got := map[string]string{}
	for _, kv := range attrs {
		got[kv.Key] = *kv.Value.IntValue
	}
	want := map[string]string{
		"gen_ai.usage.input_tokens":             "2590",
		"gen_ai.usage.cache_read.input_tokens":  "2000",
		"gen_ai.usage.cache_write.input_tokens": "500",
		"gen_ai.usage.output_tokens":            "210",
	}
	if len(got) != len(want) {
		t.Errorf("attributes = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestFailedCallSetsErrorStatus(t *testing.T) {
	c := Call{RequestModel: "m", StatusCode: 429, ErrorType: "429", Start: callStart, End: callStart.Add(time.Second)}
	s := newRecord(&c).span()
	if s.Status == nil || s.Status.Code != statusError || s.Kind != spanKindClient || s.Name != "chat m" {
		t.Errorf("span = %+v, want an ERROR CLIENT span named \"chat m\"", s)
	}
	if len(s.TraceID) != 32 || len(s.SpanID) != 16 || s.ParentSpanID != "" {
		t.Errorf("ids = %q %q %q, want a fresh trace without a parent", s.TraceID, s.SpanID, s.ParentSpanID)
	}
}

func TestMalformedTraceparentStartsANewTrace(t *testing.T) {
	for _, tp := range []string{"", "garbage", "00-00000000000000000000000000000000-b7ad6b7169203331-01", "01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"} {
		traceID, _, parent := spanIDs(tp)
		if parent != "" || len(traceID) != 32 {
			t.Errorf("%q: trace %q parent %q, want a fresh trace", tp, traceID, parent)
		}
	}
}

func TestContentGroupsAssistantTurnsAndKeepsRawArguments(t *testing.T) {
	msgs := messages([]v1.Item{
		&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
		&v1.Reasoning{Content: "think"},
		&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "calling"}}},
		&v1.FunctionCall{CallID: "c1", Name: "f", Arguments: "not json"},
		&v1.FunctionCall{CallID: "c2", Name: "g", Arguments: `{"n":1}`},
		&v1.FunctionCallOutput{CallID: "c1", Output: "a"},
		&v1.FunctionCallOutput{CallID: "c2", Output: "b"},
		&v1.Reasoning{ProviderData: json.RawMessage(`"encrypted"`)},
	})
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want user, assistant, tool", msgs)
	}
	assistant := msgs[1].(map[string]any)
	parts := assistant["parts"].([]any)
	if assistant["role"] != "assistant" || len(parts) != 4 {
		t.Fatalf("assistant message = %v, want reasoning, text and two tool calls", assistant)
	}
	if parts[2].(map[string]any)["arguments"] != "not json" {
		t.Errorf("unparseable arguments = %v, want the raw text", parts[2])
	}
	if tool := msgs[2].(map[string]any); tool["role"] != "tool" || len(tool["parts"].([]any)) != 2 {
		t.Errorf("tool message = %v, want both responses", tool)
	}
}
