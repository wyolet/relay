package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// An unknown stop_reason must not read as a clean stop (rule 11): it parses to
// incomplete/other and the raw value is kept.
func TestAnthropicParseResponse_UnknownStopReasonNotSuccess(t *testing.T) {
	body := mustJSON(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
		"content":       []any{map[string]any{"type": "text", "text": "x"}},
		"stop_reason":   "new_reason",
		"stop_sequence": "END",
		"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	resp, err := (AnthropicTranslator{}).ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther {
		t.Errorf("status/finish = %q/%q, want incomplete/other", resp.Status, resp.FinishReason)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "anthropic:new_reason" {
		t.Errorf("incomplete_details = %+v", resp.IncompleteDetails)
	}
	if got := string(resp.Extensions["anthropic.stop_reason"]); got != `"new_reason"` {
		t.Errorf(`extensions["anthropic.stop_reason"] = %s`, got)
	}
	if _, ok := resp.Extensions["stop_sequence"]; !ok {
		t.Error("stop_sequence extension lost alongside the raw stop_reason")
	}

	out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeMap(t, out)["stop_reason"]; got != "new_reason" {
		t.Errorf("round-trip stop_reason = %v, want the raw value, not end_turn", got)
	}
}

func TestAnthropicToCanonical_UnknownStopReasonNotSuccess(t *testing.T) {
	fn := (AnthropicTranslator{}).NewToCanonicalStream()
	var frames [][]byte
	for _, c := range [][]byte{
		messageStartChunk("msg_u", "claude-x"),
		contentBlockStartText(0),
		textDeltaChunk(0, "x"),
		contentBlockStopChunk(0),
		messageDeltaChunk("new_reason", 1),
		messageStopChunk(),
	} {
		out, err := fn(c)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, splitFrames(out)...)
	}
	for _, f := range frames {
		if ev, data, _ := v1.ParseSSEChunk(f); ev == v1.EventGenerationCompleted {
			var ge v1.GenerationCompletedEvent
			if err := json.Unmarshal(data, &ge); err != nil {
				t.Fatal(err)
			}
			if ge.Status != v1.StatusIncomplete || ge.FinishReason != v1.FinishReasonOther {
				t.Errorf("status/finish = %q/%q, want incomplete/other", ge.Status, ge.FinishReason)
			}
			if ge.IncompleteDetails == nil || ge.IncompleteDetails.Reason != "anthropic:new_reason" {
				t.Errorf("incomplete_details = %+v, want the raw reason", ge.IncompleteDetails)
			}
			return
		}
	}
	t.Fatal("no generation.completed frame")
}

// canonical other → Anthropic stop_reason, buffered and streamed. Only an
// Anthropic upstream's own raw reason is written back; nothing becomes end_turn.
func TestAnthropicSerialize_OtherFinishReason(t *testing.T) {
	cases := []struct {
		name string
		inc  *v1.IncompleteDetails
		want string
	}{
		{"own raw reason", &v1.IncompleteDetails{Reason: "anthropic:new_reason"}, "new_reason"},
		{"foreign raw reason", &v1.IncompleteDetails{Reason: "openai:new_reason"}, "refusal"},
		{"unprefixed reason", &v1.IncompleteDetails{Reason: "new_reason"}, "refusal"},
		{"no details", nil, "refusal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &v1.Response{ID: "m1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc}
			out, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodeMap(t, out)["stop_reason"]; got != tc.want {
				t.Errorf("buffered stop_reason = %v, want %s", got, tc.want)
			}

			fn := (AnthropicTranslator{}).NewFromCanonicalStream()
			completed := mustJSON(v1.GenerationCompletedEvent{ID: "m1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc})
			wire, err := fn([]byte("event: " + v1.EventGenerationCompleted + "\ndata: " + string(completed) + "\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			if want := `"stop_reason":"` + tc.want + `"`; !strings.Contains(string(wire), want) {
				t.Errorf("stream wire lacks %s:\n%s", want, wire)
			}
		})
	}
}
