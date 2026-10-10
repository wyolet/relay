package openai

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Raw reasoning text rides a reasoning item's content array as reasoning_text parts, next to summary; open-weight reasoning models served over Responses return it in plaintext.
const reasoningContentWire = `"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}]`

func TestResponsesReasoningContent_Response(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-oss-120b","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Checking"}],"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}]}]}`
	resp, err := ResponsesTranslator{}.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := resp.Output[0].(*v1.Reasoning)
	if !ok {
		t.Fatalf("output[0] = %T, want *v1.Reasoning", resp.Output[0])
	}
	if r.Content != "221 = 13 * 17" {
		t.Fatalf("parsed Reasoning.Content = %q", r.Content)
	}
	out, err := ResponsesTranslator{}.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), reasoningContentWire) {
		t.Fatalf("serialized response lacks reasoning content: %s", out)
	}
}

func TestResponsesReasoningContent_RequestInput(t *testing.T) {
	body := `{"model":"gpt-oss-120b","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Is 221 prime?"}]},{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}],"encrypted_content":"BLOB"}]}`
	req, err := ResponsesTranslator{}.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := req.Input[1].(*v1.Reasoning)
	if !ok {
		t.Fatalf("input[1] = %T, want *v1.Reasoning", req.Input[1])
	}
	if r.Content != "221 = 13 * 17" {
		t.Fatalf("parsed Reasoning.Content = %q", r.Content)
	}
	out, err := ResponsesTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), reasoningContentWire) {
		t.Fatalf("serialized request lacks reasoning content: %s", out)
	}
}

func TestResponsesReasoningContent_ToCanonicalStream(t *testing.T) {
	toCanon := ResponsesTranslator{}.NewToCanonicalStream()
	chunks := []string{
		`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-oss-120b","status":"in_progress"}}`,
		`event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"221 = 13 * 17"}`,
		`event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"221 = 13 * 17"}]}}`,
	}
	var all strings.Builder
	for i, c := range chunks {
		out, err := toCanon([]byte(c + "\n\n"))
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		all.Write(out)
	}
	if !strings.Contains(all.String(), `"content":"221 = 13 * 17"`) {
		t.Fatalf("terminal reasoning item lacks content: %s", all.String())
	}
}

func TestResponsesReasoningContent_FromCanonicalStream(t *testing.T) {
	fromCanon := ResponsesTranslator{}.NewFromCanonicalStream()
	chunks := []string{
		"event: generation.created\ndata: {\"id\":\"gen_1\",\"model\":\"m\"}\n\n",
		"event: item.started\ndata: {\"item_id\":\"rs_1\",\"item_type\":\"reasoning\",\"index\":0}\n\n",
		"event: item.delta\ndata: {\"item_id\":\"rs_1\",\"index\":0,\"kind\":\"reasoning\",\"delta\":\"221 = 13 * 17\"}\n\n",
		"event: item.completed\ndata: {\"item_id\":\"rs_1\",\"index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[{\"text\":\"221 = 13 * 17\"}],\"content\":\"221 = 13 * 17\"}}\n\n",
		"event: generation.completed\ndata: {\"id\":\"gen_1\",\"status\":\"completed\",\"finish_reason\":\"stop\"}\n\n",
	}
	var all strings.Builder
	for i, c := range chunks {
		out, err := fromCanon([]byte(c))
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		all.Write(out)
	}
	out := all.String()
	for _, ev := range []string{ResponsesEventOutputItemDone, ResponsesEventCompleted} {
		i := strings.Index(out, "event: "+ev+"\n")
		if i < 0 {
			t.Fatalf("no %s event: %s", ev, out)
		}
		frame, _, _ := strings.Cut(out[i:], "\n\n")
		if !strings.Contains(frame, reasoningContentWire) {
			t.Errorf("%s reasoning item lacks content: %s", ev, frame)
		}
	}
}
