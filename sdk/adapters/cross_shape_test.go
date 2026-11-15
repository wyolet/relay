package adapters_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "github.com/wyolet/relay/sdk/v1"
)

const (
	answerSchema    = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	chatSchemaReq   = `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":` + answerSchema + `,"strict":true}}`
	messagesFormat  = `"output_config":{"format":{"type":"json_schema","schema":` + answerSchema + `}}`
	messagesHiTurns = `"messages":[{"role":"user","content":"hi"}]`
	chatTwoTools    = `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"}}},{"type":"function","function":{"name":"b","parameters":{"type":"object"}}}]`
	geminiHiTurn    = `"contents":[{"role":"user","parts":[{"text":"hi"}]}]`
)

// A request parsed from one shape serializes to another shape's exact wire body, or is refused.
func TestCrossShapeRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		body     string
		want     string // empty: the target refuses with ErrUnsupportedRequest
	}{
		{
			name: "chat json_schema reaches Messages as output_config.format, with no tool",
			from: "openai-chat", to: "anthropic",
			body: chatSchemaReq + `}`,
			want: `{"model":"m","max_tokens":4096,` + messagesHiTurns + `,` + messagesFormat + `}`,
		},
		{
			name: "responses json_schema reaches Messages as output_config.format",
			from: "openai-responses", to: "anthropic",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"text":{"format":{"type":"json_schema","name":"answer","schema":` + answerSchema + `,"strict":true}}}`,
			want: `{"model":"m","max_tokens":4096,` + messagesHiTurns + `,` + messagesFormat + `}`,
		},
		{
			name: "chat json_object has no Messages form",
			from: "openai-chat", to: "anthropic",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
		},
		{
			name: "chat reasoning effort and json_schema share one output_config",
			from: "openai-chat", to: "anthropic",
			body: chatSchemaReq + `,"reasoning_effort":"high"}`,
			want: `{"model":"m","max_tokens":4096,` + messagesHiTurns + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high","format":{"type":"json_schema","schema":` + answerSchema + `}}}`,
		},
		{
			name: "chat forced tool choice survives beside json_schema",
			from: "openai-chat", to: "anthropic",
			body: chatSchemaReq + `,"tools":[{"type":"function","function":{"name":"lookup","description":"look up","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
			want: `{"model":"m","max_tokens":4096,` + messagesHiTurns + `,"tools":[{"name":"lookup","description":"look up","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"},` + messagesFormat + `}`,
		},
		{
			name: "chat allowed_tools reaches Messages as only the allowed tools",
			from: "openai-chat", to: "anthropic",
			body: chatTwoTools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"b"}}]}}}`,
			want: `{"model":"m","max_tokens":4096,` + messagesHiTurns + `,"tools":[{"name":"b","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}`,
		},
		{
			name: "chat required allowed_tools reaches Gemini as allowedFunctionNames",
			from: "openai-chat", to: "gemini",
			body: chatTwoTools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"b"}},{"type":"function","function":{"name":"a"}}]}}}`,
			want: `{` + geminiHiTurn + `,"tools":[{"functionDeclarations":[{"name":"a","parameters":{"type":"object"}},{"name":"b","parameters":{"type":"object"}}]}],
				"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["b","a"]}}}`,
		},
		{
			name: "chat auto allowed_tools reaches Gemini as only the allowed declarations",
			from: "openai-chat", to: "gemini",
			body: chatTwoTools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"function","function":{"name":"b"}}]}}}`,
			want: `{` + geminiHiTurn + `,"tools":[{"functionDeclarations":[{"name":"b","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`,
		},
		{
			name: "responses json_schema and effort reach chat",
			from: "openai-responses", to: "openai-chat",
			body: `{"model":"m","input":"hi","reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","name":"answer","schema":` + answerSchema + `,"strict":true}}}`,
			want: chatSchemaReq + `,"reasoning_effort":"high"}`,
		},
		{
			name: "Messages format reaches Gemini as responseSchema",
			from: "anthropic", to: "gemini",
			body: `{"model":"m","max_tokens":1024,` + messagesHiTurns + `,` + messagesFormat + `}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1024,"responseMimeType":"application/json","responseSchema":` + answerSchema + `}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := shapeByName(tc.from).tr.ParseRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := shapeByName(tc.to).tr.SerializeRequest(req)
			if tc.want == "" {
				if !errors.Is(err, v1.ErrUnsupportedRequest) {
					t.Fatalf("err = %v (body %s), want ErrUnsupportedRequest", err, wire)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(decodeJSON(t, []byte(tc.want)), decodeJSON(t, wire)); diff != "" {
				t.Errorf("wire (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCrossShapeRequest_NamelessSchemaGetsAName(t *testing.T) {
	req, err := shapeByName("anthropic").tr.ParseRequest([]byte(`{"model":"m","max_tokens":1024,` + messagesHiTurns + `,` + messagesFormat + `}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"openai-chat", "openai-responses"} {
		wire, err := shapeByName(to).tr.SerializeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			ResponseFormat struct {
				JSONSchema struct{ Name string } `json:"json_schema"`
			} `json:"response_format"`
			Text struct{ Format struct{ Name string } }
		}
		if err := json.Unmarshal(wire, &body); err != nil {
			t.Fatal(err)
		}
		if body.ResponseFormat.JSONSchema.Name+body.Text.Format.Name == "" {
			t.Errorf("%s: schema name is empty: %s", to, wire)
		}
	}
}

const (
	messagesJSONAnswer = `{"id":"msg_01","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"{\"answer\":\"Paris\"}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	messagesJSONStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"model\":\"m\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"answer\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"\\\"Paris\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// A Messages structured-output answer is a plain text block; chat clients get it as message content with a clean stop and no tool call.
func TestCrossShapeMessagesJSONAnswerToChat(t *testing.T) {
	type chatBody struct {
		Choices []struct {
			Message, Delta struct {
				Content   string `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			}
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	type answer struct {
		Content, FinishReason string
		ToolCalls             int
	}
	want := answer{Content: `{"answer":"Paris"}`, FinishReason: "stop"}
	messages, chat := shapeByName("anthropic").tr, shapeByName("openai-chat").tr

	resp, err := messages.ParseResponse([]byte(messagesJSONAnswer))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := chat.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buffered chatBody
	if err := json.Unmarshal(wire, &buffered); err != nil || len(buffered.Choices) != 1 {
		t.Fatalf("chat body %s: %v", wire, err)
	}
	c := buffered.Choices[0]
	if got := (answer{c.Message.Content, c.FinishReason, len(c.Message.ToolCalls)}); got != want {
		t.Errorf("buffered = %+v, want %+v", got, want)
	}

	canon, err := translateFrames(messages.NewToCanonicalStream(), splitFrames([]byte(messagesJSONStream)))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := translateFrames(chat.NewFromCanonicalStream(), canon)
	if err != nil {
		t.Fatal(err)
	}
	var streamed answer
	for _, f := range recordFrames(chunks) {
		if string(f.Data) == `"[DONE]"` {
			continue
		}
		var chunk chatBody
		if err := json.Unmarshal(f.Data, &chunk); err != nil {
			t.Fatalf("chat chunk %s: %v", f.Data, err)
		}
		for _, c := range chunk.Choices {
			streamed.Content += c.Delta.Content
			streamed.FinishReason += c.FinishReason
			streamed.ToolCalls += len(c.Delta.ToolCalls)
		}
	}
	if streamed != want {
		t.Errorf("streamed = %+v, want %+v\n%s", streamed, want, strings.Join(frameStrings(chunks), ""))
	}
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return v
}

func frameStrings(frames [][]byte) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = string(f)
	}
	return out
}
