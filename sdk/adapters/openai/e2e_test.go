package openai

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A Responses caller served by a Chat Completions upstream: request out, response back with the request's echo fields.
func TestE2E_ResponsesViaCC(t *testing.T) {
	rt, cc := ResponsesTranslator{}, CCTranslator{}
	req, err := rt.ParseRequest([]byte(`{"model":"gpt-4o","input":"hello","temperature":0.5}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := cc.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, wire, `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"temperature":0.5}`)

	resp, err := cc.ParseResponse([]byte(ccResponse(`{"index":0,"message":{"role":"assistant","content":"Hello there!"},"finish_reason":"stop"}`,
		`,"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}`)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := rt.SerializeResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object, Status, Model string
		Temperature           float64
		Output                []struct {
			Type    string
			Content []struct{ Type, Text string }
		}
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "response" || got.Status != "completed" || got.Model != "m" || got.Temperature != 0.5 ||
		len(got.Output) != 1 || got.Output[0].Type != "message" || len(got.Output[0].Content) != 1 ||
		got.Output[0].Content[0] != (struct{ Type, Text string }{"output_text", "Hello there!"}) ||
		got.Usage.InputTokens != 5 || got.Usage.OutputTokens != 3 {
		t.Errorf("Responses body: %s", body)
	}
}

// A Responses request body as a Chat Completions upstream receives it.
func TestE2E_ResponsesRequestToCC(t *testing.T) {
	cases := []struct {
		name, body, wire string
	}{
		{
			name: "type-less message items",
			body: `{"model":"gpt-4o","input":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"},
				{"role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"role":"user","content":[{"type":"input_text","text":"again"}]}]}`,
			wire: `{"model":"gpt-4o","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},
				{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ResponsesTranslator{}.ParseRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := CCTranslator{}.SerializeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			assertJSON(t, wire, tc.wire)
		})
	}
}

func TestE2E_StreamingCCToResponses(t *testing.T) {
	cs := NewComposedStream(CCTranslator{}.NewToCanonicalStream(), ResponsesTranslator{}.NewFromCanonicalStream())
	var events []string
	var text string
	for _, f := range []string{ccChunk(`{"role":"assistant","content":"Hello"}`), ccChunk(`{"content":" world"}`), "data: [DONE]"} {
		frames, err := cs.Translate([]byte(f + "\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range frames {
			events = append(events, fr.Event)
			if fr.Event == ResponsesEventOutputTextDone {
				var done struct{ Text string }
				if err := json.Unmarshal(fr.Data, &done); err != nil {
					t.Fatal(err)
				}
				text = done.Text
			}
		}
	}
	want := []string{
		ResponsesEventCreated, ResponsesEventInProgress, ResponsesEventOutputItemAdded, ResponsesEventContentPartAdded,
		ResponsesEventOutputTextDelta, ResponsesEventOutputTextDelta, ResponsesEventOutputTextDone,
		ResponsesEventContentPartDone, ResponsesEventOutputItemDone, ResponsesEventCompleted,
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v\nwant     %v", events, want)
	}
	if text != "Hello world" {
		t.Errorf("output_text.done text = %q, want the joined deltas", text)
	}
}
