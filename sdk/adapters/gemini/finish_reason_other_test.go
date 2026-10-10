package gemini_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/gemini"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// An unrecognised finishReason parses to incomplete/other with the raw value
// kept, buffered and streamed.
func TestGeminiParse_UnknownFinishReasonIsOther(t *testing.T) {
	for _, reason := range []string{"NEW_REASON", "OTHER", "FINISH_REASON_UNSPECIFIED"} {
		t.Run(reason, func(t *testing.T) {
			body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"` + reason + `","index":0}],"modelVersion":"g"}`
			resp, err := gemini.GeminiTranslator{}.ParseResponse([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			want := "gemini:" + reason
			if resp.Status != v1.StatusIncomplete || resp.FinishReason != v1.FinishReasonOther || resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != want {
				t.Errorf("buffered = %q/%q/%+v, want incomplete/other/%s", resp.Status, resp.FinishReason, resp.IncompleteDetails, want)
			}

			canon, err := gemini.GeminiTranslator{}.NewToCanonicalStream()([]byte("data: " + body + "\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			var done v1.GenerationCompletedEvent
			for _, f := range strings.Split(string(canon), "\n\n") {
				if ev, data, ok := v1.ParseSSEChunk([]byte(f)); ok && ev == v1.EventGenerationCompleted {
					if err := json.Unmarshal(data, &done); err != nil {
						t.Fatal(err)
					}
				}
			}
			if done.Status != v1.StatusIncomplete || done.FinishReason != v1.FinishReasonOther || done.IncompleteDetails == nil || done.IncompleteDetails.Reason != want {
				t.Errorf("stream = %q/%q/%+v, want incomplete/other/%s", done.Status, done.FinishReason, done.IncompleteDetails, want)
			}
		})
	}
}

// canonical other → Gemini finishReason, buffered and streamed. Only a Gemini
// upstream's own raw reason is written back; everything else is OTHER.
func TestGeminiSerialize_OtherFinishReason(t *testing.T) {
	cases := []struct {
		name string
		inc  *v1.IncompleteDetails
		want string
	}{
		{"own raw reason", &v1.IncompleteDetails{Reason: "gemini:NEW_REASON"}, "NEW_REASON"},
		{"foreign raw reason", &v1.IncompleteDetails{Reason: "openai:new_reason"}, "OTHER"},
		{"no details", nil, "OTHER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &v1.Response{ID: "g1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc}
			out, err := gemini.GeminiTranslator{}.SerializeResponse(resp, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := `"finishReason":"` + tc.want + `"`
			if !strings.Contains(string(out), want) {
				t.Errorf("buffered body lacks %s:\n%s", want, out)
			}

			completed, _ := json.Marshal(v1.GenerationCompletedEvent{ID: "g1", Status: v1.StatusIncomplete, FinishReason: v1.FinishReasonOther, IncompleteDetails: tc.inc})
			wire, err := gemini.GeminiTranslator{}.NewFromCanonicalStream()([]byte("event: " + v1.EventGenerationCompleted + "\ndata: " + string(completed) + "\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(wire), want) {
				t.Errorf("stream wire lacks %s:\n%s", want, wire)
			}
		})
	}
}
