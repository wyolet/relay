package openai

import (
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// translateAll hides a translator's v1.SummaryStream so summaries built on it
// translate every frame.
type translateAll struct{ v1.Translator }

// checkSummaryFrames feeds frames through one summary stream, checking the
// predicate's verdict on each, then checks that the filtered summaries equal
// the unfiltered ones and returns the filtered one.
func checkSummaryFrames(t *testing.T, tr v1.Translator, frames []string, want []bool) v1.Summary {
	t.Helper()
	toCanon, affects := tr.(v1.SummaryStream).NewSummaryStream()
	for i, f := range frames {
		got := affects([]byte(f))
		if got != want[i] {
			t.Errorf("frame %d %q: affectsSummary = %v, want %v", i, f, got, want[i])
		}
		if got {
			_, _ = toCanon([]byte(f + "\n\n"))
		}
	}
	body := []byte(strings.Join(frames, "\n\n") + "\n\n")
	filtered, _ := v1.ExtractSummary(tr, body)
	all, _ := v1.ExtractSummary(translateAll{tr}, body)
	if !reflect.DeepEqual(filtered, all) {
		t.Errorf("ExtractSummary: filtered %+v, unfiltered %+v", filtered, all)
	}
	fs, as := v1.NewStreamSummarizer(tr), v1.NewStreamSummarizer(translateAll{tr})
	for _, f := range frames {
		fs.Observe([]byte(f))
		as.Observe([]byte(f))
	}
	if !reflect.DeepEqual(fs.Summary(), as.Summary()) {
		t.Errorf("StreamSummarizer: filtered %+v, unfiltered %+v", fs.Summary(), as.Summary())
	}
	if !reflect.DeepEqual(fs.Summary(), filtered) {
		t.Errorf("StreamSummarizer %+v, ExtractSummary %+v", fs.Summary(), filtered)
	}
	return filtered
}

func TestCCSummaryFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"content delta", `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":null,"finish_reason":null}]}`, false},
		{"nulls with spaced keys", `data: { "usage" : null , "choices" : [ { "delta" : { "content" : "hi" , "refusal" : null } , "finish_reason" : null } ] }`, false},
		{"empty refusal and finish", `data: {"choices":[{"delta":{"refusal":""},"finish_reason":""}]}`, false},
		{"usage named in content", `data: {"choices":[{"delta":{"content":"\"usage\":{\"x\":1}, \"finish_reason\":\"stop\""}}]}`, false},
		{"tool-call delta", `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]},"finish_reason":null}]}`, false},
		{"reasoning delta", `data: {"choices":[{"delta":{"reasoning_content":"think"},"finish_reason":null}]}`, false},
		{"second choice finishing", `data: {"choices":[{"delta":{"content":"a"}},{"delta":{},"finish_reason":"stop"}]}`, false},
		{"DONE", `data: [DONE]`, true},
		{"DONE without space", `data:[DONE]`, true},
		{"usage-only chunk", `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, true},
		{"usage spaced", `data: {"usage" :{"prompt_tokens":1}, "choices":[]}`, true},
		{"usage key case", `data: {"Usage":{"prompt_tokens":1}}`, true},
		{"usage key escaped", `data: {"usage":{"prompt_tokens":1}}`, true},
		{"usage CRLF", "data: {\"choices\":[],\r\ndata: \"usage\":{\"prompt_tokens\":1}}", true},
		{"finish first in choice", `data: {"choices":[{"finish_reason":"stop","delta":{},"index":0}]}`, true},
		{"refusal delta", `data: {"choices":[{"delta":{"refusal":"I can't"},"finish_reason":null}]}`, true},
		{"error", `data: {"error":{"message":"overloaded","type":"server_error"}}`, true},
		{"error null", `data: {"error":null,"choices":[{"delta":{"content":"a"}}]}`, false},
		{"first tier", `data: {"service_tier":"default","choices":[{"delta":{"content":"a"}}]}`, true},
		{"truncated", `data: {"choices":[{"delta":{"content":"a`, true},
		{"event-typed", "event: chunk\ndata: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, affects := CCTranslator{}.NewSummaryStream()
			if got := affects([]byte(tc.frame)); got != tc.want {
				t.Errorf("affectsSummary = %v, want %v", got, tc.want)
			}
		})
	}
}

// Frame sequences whose verdicts depend on what earlier frames applied.
func TestCCSummaryFrameSequence(t *testing.T) {
	tier := func(tier, delta string) string {
		return `data: {"id":"c","service_tier":"` + tier + `","choices":[{"index":0,"delta":{"content":"` + delta + `"},"finish_reason":null}]}`
	}
	cases := []struct {
		name    string
		frames  []string
		affects []bool
		want    v1.Summary
	}{
		{"a tier counts only when it changes",
			[]string{tier("default", "a"), tier("default", "b"), tier("flex", "c"), tier("flex", "d"), tier("default", "e"), `data: [DONE]`},
			[]bool{true, false, true, false, true, true},
			v1.Summary{FinishReason: v1.FinishReasonStop, ServiceTier: "default"}},
		{"only the first refusal delta counts",
			[]string{
				`data: {"choices":[{"delta":{"role":"assistant","refusal":""}}]}`,
				`data: {"choices":[{"delta":{"refusal":"I can't"}}]}`,
				`data: {"choices":[{"delta":{"refusal":" help."}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			},
			[]bool{false, true, false, true, true},
			v1.Summary{FinishReason: v1.FinishReasonRefusal}},
		{"nothing counts after an error",
			[]string{
				`data: {"choices":[{"delta":{"content":"a"}}]}`,
				`data: {"error":{"message":"overloaded"}}`,
				`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
				`data: [DONE]`,
			},
			[]bool{false, true, false, false},
			v1.Summary{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkSummaryFrames(t, CCTranslator{}, tc.frames, tc.affects); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("summary = %+v, want %+v", got, tc.want)
			}
		})
	}
}
