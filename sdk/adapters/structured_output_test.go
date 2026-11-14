package adapters_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// schema used throughout.
var soSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)

// jsonEqual reports whether two JSON byte slices are semantically equal.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// assertOutputConfigFormat checks that a serialized Anthropic wire body has
// output_config.format.type == wantType and that schema is semantically equal
// to wantSchema. When noCallerTools is true it also verifies that no synthetic
// tool was injected (the old forced-tool path).
func assertOutputConfigFormat(t *testing.T, wire []byte, wantType string, wantSchema json.RawMessage) {
	t.Helper()
	assertOutputConfigFormatFull(t, wire, wantType, wantSchema, true)
}

func assertOutputConfigFormatFull(t *testing.T, wire []byte, wantType string, wantSchema json.RawMessage, checkNoTools bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	oc, _ := m["output_config"].(map[string]any)
	if oc == nil {
		t.Fatalf("output_config missing; wire = %s", wire)
	}
	f, _ := oc["format"].(map[string]any)
	if f == nil {
		t.Fatalf("output_config.format missing; wire = %s", wire)
	}
	if got := f["type"]; got != wantType {
		t.Errorf("output_config.format.type = %q, want %q", got, wantType)
	}
	if wantSchema != nil {
		schemaRaw, _ := json.Marshal(f["schema"])
		if !jsonEqual(schemaRaw, wantSchema) {
			t.Errorf("output_config.format.schema = %s, want %s", schemaRaw, wantSchema)
		}
	}
	if checkNoTools {
		if tools, _ := m["tools"].([]any); len(tools) > 0 {
			t.Errorf("unexpected tools in output: %v", tools)
		}
		if _, hasTC := m["tool_choice"]; hasTC {
			t.Errorf("unexpected tool_choice in output: %v", m["tool_choice"])
		}
	}
}

// --- tests 1–2: inbound → Anthropic wire ---

// TestCrossShape_CCJsonSchemaToAnthropic proves an OpenAI CC json_schema request
// reaches Anthropic as output_config.format, without a synthetic forced tool.
func TestCrossShape_CCJsonSchemaToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "answer",
				"schema": ` + string(soSchema) + `,
				"strict": true
			}
		}
	}`)
	req, err := openai.CCTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.AnthropicTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertOutputConfigFormat(t, wire, "json_schema", soSchema)
}

// TestCrossShape_CCJsonObjectToAnthropic proves json_object, which has no
// Anthropic form, is refused instead of reaching the upstream.
func TestCrossShape_CCJsonObjectToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {"type": "json_object"}
	}`)
	req, err := openai.CCTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (anthropic.AnthropicTranslator{}).SerializeRequest(req); !errors.Is(err, v1.ErrUnsupportedRequest) {
		t.Fatalf("json_object to Anthropic: err = %v, want ErrUnsupportedRequest", err)
	}
}

// TestCrossShape_ResponsesJsonSchemaToAnthropic proves an OpenAI Responses
// json_schema request reaches Anthropic as output_config.format.
func TestCrossShape_ResponsesJsonSchemaToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
		"text": {
			"format": {
				"type": "json_schema",
				"name": "answer",
				"schema": ` + string(soSchema) + `,
				"strict": true
			}
		}
	}`)
	req, err := openai.ResponsesTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.AnthropicTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertOutputConfigFormat(t, wire, "json_schema", soSchema)
}

// --- test 3: Anthropic inbound → OpenAI CC / Gemini ---

// TestCrossShape_AnthropicToCC proves an Anthropic request with
// output_config.format serializes to OpenAI CC response_format json_schema
// carrying the same schema.
func TestCrossShape_AnthropicToCC(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hi"}],
		"output_config": {
			"effort": "high",
			"format": {"type": "json_schema", "schema": ` + string(soSchema) + `}
		}
	}`)
	req, err := anthropic.AnthropicTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := openai.CCTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("unmarshal cc wire: %v", err)
	}
	rf, _ := m["response_format"].(map[string]any)
	if rf == nil {
		t.Fatalf("response_format missing; wire = %s", wire)
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	inner, _ := json.Marshal(rf["json_schema"])
	var js struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(inner, &js); err != nil || !jsonEqual(js.Schema, soSchema) {
		t.Errorf("response_format.json_schema.schema = %s, want %s", js.Schema, soSchema)
	}
}

// TestCrossShape_AnthropicToGemini proves an Anthropic request with
// output_config.format lands on Gemini's responseSchema field.
func TestCrossShape_AnthropicToGemini(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hi"}],
		"output_config": {
			"format": {"type": "json_schema", "schema": ` + string(soSchema) + `}
		}
	}`)
	req, err := anthropic.AnthropicTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := gemini.GeminiTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("unmarshal gemini wire: %v", err)
	}
	gc, _ := m["generationConfig"].(map[string]any)
	if gc == nil {
		t.Fatalf("generationConfig missing; wire = %s", wire)
	}
	schemaRaw, _ := json.Marshal(gc["responseSchema"])
	if !jsonEqual(schemaRaw, soSchema) {
		t.Errorf("responseSchema = %s, want %s", schemaRaw, soSchema)
	}
	if gc["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType = %v, want application/json", gc["responseMimeType"])
	}
}

// --- test 4: response direction, sync ---

// TestCrossShape_AnthropicNativeResponseToCC proves an Anthropic native
// structured-output response (text block, stop_reason end_turn) round-trips
// through canonical to CC with the JSON as message content and
// finish_reason "stop"; no tool_calls emitted.
func TestCrossShape_AnthropicNativeResponseToCC(t *testing.T) {
	const jsonPayload = `{"answer":"Paris"}`
	// Embed jsonPayload as a properly escaped JSON string.
	textVal, _ := json.Marshal(jsonPayload)
	body, _ := json.Marshal(map[string]any{
		"id":          "msg_01",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-5-5",
		"content":     []any{map[string]any{"type": "text", "text": jsonPayload}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	_ = textVal
	canon, err := anthropic.AnthropicTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	ccWire, err := openai.CCTranslator{}.SerializeResponse(canon, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ccResp struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []any   `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(ccWire, &ccResp); err != nil {
		t.Fatalf("unmarshal cc response: %v", err)
	}
	if len(ccResp.Choices) == 0 {
		t.Fatalf("no choices; wire = %s", ccWire)
	}
	ch := ccResp.Choices[0]
	if ch.Message.Content == nil || *ch.Message.Content != jsonPayload {
		t.Errorf("message.content = %v, want %q", ch.Message.Content, jsonPayload)
	}
	if ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) > 0 {
		t.Errorf("unexpected tool_calls: %v", ch.Message.ToolCalls)
	}
}

// --- test 5: response direction, stream ---

// anthropicTextStream builds a minimal Anthropic SSE stream that delivers
// payload as a text block.
func anthropicTextStream(id, payload string) []byte {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"" + id + "\",\"model\":\"claude-sonnet-5-5\",\"usage\":{\"input_tokens\":10}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	}
	// Split payload across two deltas to verify concatenation.
	half := len(payload) / 2
	if half == 0 {
		half = 1
	}
	p1, p2 := payload[:half], payload[half:]
	frames = append(frames,
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":"+mustMarshalStr(p1)+"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":"+mustMarshalStr(p2)+"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	return []byte(strings.Join(frames, ""))
}

func mustMarshalStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestCrossShape_AnthropicStreamToCC proves an Anthropic SSE stream of a text
// block translates to canonical and then to CC: concatenated content deltas
// equal the original payload and the final chunk carries finish_reason "stop".
func TestCrossShape_AnthropicStreamToCC(t *testing.T) {
	const jsonPayload = `{"answer":"Paris"}`
	body := anthropicTextStream("msg_01", jsonPayload)

	toCanon := anthropic.AnthropicTranslator{}.NewToCanonicalStream()
	fromCanon := openai.CCTranslator{}.NewFromCanonicalStream()

	var contentBuf strings.Builder
	var finalFinishReason string

	for _, frame := range splitFrames(body) {
		canon, err := toCanon(frame)
		if err != nil {
			t.Fatalf("toCanon: %v", err)
		}
		ccFrames := splitFrames(canon)
		for _, cf := range ccFrames {
			out, err := fromCanon(cf)
			if err != nil {
				t.Fatalf("fromCanon: %v", err)
			}
			for _, of := range splitFrames(out) {
				data := extractSSEData(of)
				if data == "[DONE]" {
					continue
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content *string `json:"content"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(data), &chunk); err != nil {
					continue
				}
				for _, ch := range chunk.Choices {
					if ch.Delta.Content != nil {
						contentBuf.WriteString(*ch.Delta.Content)
					}
					if ch.FinishReason != nil && *ch.FinishReason != "" {
						finalFinishReason = *ch.FinishReason
					}
				}
			}
		}
	}

	if contentBuf.String() != jsonPayload {
		t.Errorf("accumulated content = %q, want %q", contentBuf.String(), jsonPayload)
	}
	if finalFinishReason != "stop" {
		t.Errorf("final finish_reason = %q, want stop", finalFinishReason)
	}
}

// extractSSEData returns the data value of the first data: line in an SSE frame.
func extractSSEData(frame []byte) string {
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(line[len("data:"):])
		}
	}
	return ""
}

// --- test 6: thinking + format ---

// TestCrossShape_ThinkingAndFormatToAnthropic proves that a CC request with
// reasoning_effort and response_format json_schema produces a single
// output_config holding both effort and format, with thinking enabled, and no
// forced tool_choice.
func TestCrossShape_ThinkingAndFormatToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"reasoning_effort": "high",
		"messages": [{"role": "user", "content": "hi"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "answer",
				"schema": ` + string(soSchema) + `,
				"strict": true
			}
		}
	}`)
	req, err := openai.CCTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.AnthropicTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}

	oc, _ := m["output_config"].(map[string]any)
	if oc == nil {
		t.Fatalf("output_config missing; wire = %s", wire)
	}
	if oc["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", oc["effort"])
	}
	f, _ := oc["format"].(map[string]any)
	if f == nil || f["type"] != "json_schema" {
		t.Errorf("output_config.format = %v, want {type:json_schema,...}", oc["format"])
	}

	thinking, _ := m["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] == "" {
		t.Errorf("thinking block missing; wire = %s", wire)
	}

	if _, hasTC := m["tool_choice"]; hasTC {
		t.Errorf("unexpected tool_choice; wire = %s", wire)
	}
}

// --- test 7: caller tool + format ---

// TestCrossShape_CallerToolAndFormatToAnthropic proves that a CC request with
// tools, a forced function tool_choice, and response_format json_schema
// produces exactly one (caller) tool on the wire, the caller's tool_choice
// {type:tool, name:...}, and output_config.format with the schema.
func TestCrossShape_CallerToolAndFormatToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-5-5",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "lookup",
				"description": "look something up",
				"parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}
			}
		}],
		"tool_choice": {"type": "function", "function": {"name": "lookup"}},
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "answer",
				"schema": ` + string(soSchema) + `,
				"strict": true
			}
		}
	}`)
	req, err := openai.CCTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.AnthropicTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(wire, &m); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}

	// Exactly one tool — the caller's, not any synthetic one.
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools count = %d, want 1; wire = %s", len(tools), wire)
	}

	// tool_choice must be the caller's forced choice {type:tool, name:lookup}.
	tc, _ := m["tool_choice"].(map[string]any)
	if tc == nil {
		t.Fatalf("tool_choice missing; wire = %s", wire)
	}
	if tc["type"] != "tool" {
		t.Errorf("tool_choice.type = %v, want tool", tc["type"])
	}
	if tc["name"] != "lookup" {
		t.Errorf("tool_choice.name = %v, want lookup", tc["name"])
	}

	// output_config.format must still carry the schema; skip the no-tools check
	// since test 7 intentionally has the caller's tool.
	assertOutputConfigFormatFull(t, wire, "json_schema", soSchema, false)
}
