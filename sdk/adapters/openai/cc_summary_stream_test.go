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
// the unfiltered ones.
func checkSummaryFrames(t *testing.T, tr v1.Translator, frames []string, want []bool) {
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

func TestCCSummaryFrameRepeatedTier(t *testing.T) {
	chunk := func(tier, delta string) string {
		return `data: {"id":"c","service_tier":"` + tier + `","choices":[{"index":0,"delta":{"content":"` + delta + `"},"finish_reason":null}]}`
	}
	frames := []string{
		chunk("default", "a"),
		chunk("default", "b"),
		chunk("flex", "c"),
		chunk("flex", "d"),
		chunk("default", "e"),
		`data: [DONE]`,
	}
	checkSummaryFrames(t, CCTranslator{}, frames, []bool{true, false, true, false, true, true})
}

func TestCCSummaryFrameRefusalOnce(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"role":"assistant","refusal":""}}]}`,
		`data: {"choices":[{"delta":{"refusal":"I can't"}}]}`,
		`data: {"choices":[{"delta":{"refusal":" help."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	checkSummaryFrames(t, CCTranslator{}, frames, []bool{false, true, false, true, true})
}

func TestCCSummaryFrameAfterError(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		`data: {"error":{"message":"overloaded"}}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`,
	}
	checkSummaryFrames(t, CCTranslator{}, frames, []bool{false, true, false, false})
}

func TestCCSummaryFrameTierWithoutUsage(t *testing.T) {
	frames := []string{
		`data: {"service_tier":"priority","choices":[{"delta":{"content":"a"}}]}`,
		`data: {"service_tier":"priority","choices":[{"delta":{"content":"b"}}]}`,
		`data: [DONE]`,
	}
	checkSummaryFrames(t, CCTranslator{}, frames, []bool{true, false, true})
}
