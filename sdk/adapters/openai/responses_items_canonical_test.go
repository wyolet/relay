package openai

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// TestResponsesItemFromCanonical_RoleDrivenTextType guards the Responses wire
// invariant: assistant content serializes as output_text, user/system content
// as input_text — regardless of the canonical part variant. A TextPart on an
// assistant turn (how inbound parsers carry history) must NOT emit input_text,
// which OpenAI rejects with "Invalid value: 'input_text'".
func TestResponsesItemFromCanonical_RoleDrivenTextType(t *testing.T) {
	partType := func(it ResponsesItem) ResponsesPartType {
		msg, ok := it.(*ResponsesMessage)
		if !ok || len(msg.Content) == 0 {
			t.Fatalf("expected non-empty ResponsesMessage, got %T", it)
		}
		return msg.Content[0].ResponsesPartType()
	}

	cases := []struct {
		name string
		item v1.Item
		want ResponsesPartType
	}{
		{"assistant TextPart -> output_text",
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
			ResponsesPartTypeOutputText},
		{"assistant OutputTextPart -> output_text",
			&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi"}}},
			ResponsesPartTypeOutputText},
		{"user TextPart -> input_text",
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
			ResponsesPartTypeInputText},
		{"system TextPart -> input_text",
			&v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: "hi"}}},
			ResponsesPartTypeInputText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := partType(responsesItemFromCanonical(tc.item)); got != tc.want {
				t.Fatalf("part type = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- ParseRequest ---

// R-4: file_citation annotations are preserved as v1.RawAnnotation and round-trip.
func TestResponsesAnnotation_FileCitationPreserved(t *testing.T) {
	body := mustJSON(map[string]any{
		"id":         "resp_fc",
		"object":     "response",
		"created_at": 1000,
		"model":      "gpt-4o",
		"status":     "completed",
		"output": []any{map[string]any{
			"type": "message",
			"id":   "msg_fc",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": "See file [1].",
				"annotations": []any{map[string]any{
					"type":    "file_citation",
					"file_id": "file_abc",
					"index":   3,
				}},
				"logprobs": []any{},
			}},
		}},
	})
	resp, err := (ResponsesTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := resp.Output[0].(*v1.Message)
	if !ok {
		t.Fatalf("output[0] is %T", resp.Output[0])
	}
	otp, ok := msg.Content[0].(*v1.OutputTextPart)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if len(otp.Annotations) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(otp.Annotations))
	}
	raw, ok := otp.Annotations[0].(*v1.RawAnnotation)
	if !ok {
		t.Fatalf("annotation is %T, want *v1.RawAnnotation", otp.Annotations[0])
	}
	if raw.Type != "file_citation" {
		t.Errorf("annotation type: %q", raw.Type)
	}
	if !strings.Contains(string(raw.JSON), "file_abc") {
		t.Errorf("file_id missing from RawAnnotation JSON: %s", raw.JSON)
	}

	// Round-trip: confirm file_citation survives SerializeResponse.
	req := &v1.Request{Model: v1.ModelRefs{"gpt-4o"}}
	b, err := (ResponsesTranslator{}).SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "file_citation") {
		t.Errorf("file_citation missing after round-trip SerializeResponse")
	}
}

// TestResponsesRawItem_RoundTrip locks verbatim re-emission: an unmodeled item
// unmarshals to ResponsesRawItem and marshals back byte-identical.
func TestResponsesRawItem_RoundTrip(t *testing.T) {
	raw := []byte(`{"type":"mcp_call","id":"mcp_0","name":"lookup","arguments":"{}","output":"ok"}`)
	item, err := responsesUnmarshalItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	ri, ok := item.(*ResponsesRawItem)
	if !ok {
		t.Fatalf("item is %T, want *ResponsesRawItem", item)
	}
	if ri.ResponsesItemType() != "mcp_call" {
		t.Errorf("type: %q", ri.ResponsesItemType())
	}
	out, err := json.Marshal(ri)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Errorf("round-trip:\n got %s\nwant %s", out, raw)
	}
}

// FunctionCallOutput must always emit exactly one of output/content. A non-nil
// empty Content (parse round-trip of "content":[], or every part dropped in
// conversion) used to take the content branch, where omitempty erased it —
// wire item with NEITHER form → 400 missing_required_parameter 'input[N].output'.
func TestResponsesFunctionCallOutput_NeverBodiless(t *testing.T) {
	cases := map[string]*ResponsesFunctionCallOutput{
		"empty output, nil content":   {CallID: "call_1"},
		"empty output, empty content": {CallID: "call_1", Content: []ResponsesPart{}},
	}
	for name, fco := range cases {
		b, err := fco.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var wire map[string]json.RawMessage
		_ = json.Unmarshal(b, &wire)
		_, hasOutput := wire["output"]
		_, hasContent := wire["content"]
		if !hasOutput && !hasContent {
			t.Errorf("%s: neither output nor content emitted: %s", name, b)
		}
	}
}
