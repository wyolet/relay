package gemini

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// translateAll hides a translator's v1.SummaryStream so summaries built on it
// translate every frame.
type translateAll struct{ v1.Translator }

const geminiUsage = `"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1,"totalTokenCount":9}`

func TestGeminiSummaryFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"text part", `data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"index":0}],` + geminiUsage + `}`, false},
		{"reordered and spaced", "data: { " + geminiUsage + " , \"candidates\" : [ { \"index\" : 0 , \"content\" : { \"role\" : \"model\" , \"parts\" : [ { \"text\" : \"hi\" } ] } } ] }", false},
		{"data without space", `data:{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, false},
		{"CRLF", "data: {\"candidates\":[{\"content\":\r\ndata: {\"parts\":[{\"text\":\"hi\"}]}}]}", false},
		{"empty finishReason", `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":""}]}`, false},
		{"finish named in text", `data: {"candidates":[{"content":{"parts":[{"text":"\"finishReason\":\"STOP\""}]}}]}`, false},
		{"functionCall null", `data: {"candidates":[{"content":{"parts":[{"text":"hi","functionCall":null}]}}]}`, false},
		{"second candidate finishing", `data: {"candidates":[{"content":{"parts":[{"text":"a"}]}},{"finishReason":"STOP"}]}`, false},
		{"finish", `data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],` + geminiUsage + `}`, true},
		{"finish key case", `data: {"candidates":[{"content":{"parts":[]},"FinishReason":"STOP"}]}`, true},
		{"content null", `data: {"candidates":[{"content":null}],` + geminiUsage + `}`, true},
		{"content absent", `data: {"candidates":[{"index":0}],` + geminiUsage + `}`, true},
		{"functionCall", `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`, true},
		{"prompt blocked", `data: {"promptFeedback":{"blockReason":"SAFETY"},` + geminiUsage + `}`, true},
		{"error", `data: {"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`, true},
		{"no candidates", `data: {` + geminiUsage + `}`, true},
		{"empty candidates", `data: {"candidates":[]}`, true},
		{"duplicate candidates", `data: {"candidates":[{"content":{"parts":[]}}],"candidates":[{"index":0}]}`, true},
		{"duplicate content", `data: {"candidates":[{"content":{"parts":[]},"content":null}]}`, true},
		{"truncated", `data: {"candidates":[{"content":{"parts":[{"text":"h`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, affects := GeminiTranslator{}.NewSummaryStream()
			if got := affects([]byte(tc.frame)); got != tc.want {
				t.Errorf("affectsSummary = %v, want %v", got, tc.want)
			}
		})
	}
}

// A function call early in the stream turns the final STOP into tool_calls,
// and a content-less frame carries the completion's usage.
func TestGeminiSummaryFrameSequences(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   v1.Summary
	}{
		{"function call then stop", []string{
			`data: {"candidates":[{"content":{"parts":[{"text":"let me check"}]}}],` + geminiUsage + `}`,
			`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{"a":1}}}]}}],` + geminiUsage + `}`,
			`data: {"candidates":[{"content":{"parts":[{"text":"done"}]}}],` + geminiUsage + `}`,
			`data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}`,
		}, v1.Summary{Tokens: usage.Tokens{"input": 8, "output": 4}, FinishReason: v1.FinishReasonToolCalls}},
		{"usage on a content-less frame", []string{
			`data: {"candidates":[{"content":{"parts":[{"text":"a"}]}}],` + geminiUsage + `}`,
			`data: {"candidates":[{"index":0}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"totalTokenCount":11}}`,
		}, v1.Summary{Tokens: usage.Tokens{"input": 8, "output": 3}, FinishReason: v1.FinishReasonStop}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(strings.Join(c.frames, "\n\n") + "\n\n")
			for _, translator := range []v1.Translator{GeminiTranslator{}, translateAll{GeminiTranslator{}}} {
				got, err := v1.ExtractSummary(translator, body)
				if err != nil {
					t.Fatalf("%T: %v", translator, err)
				}
				if diff := cmp.Diff(c.want, got); diff != "" {
					t.Errorf("%T summary (-want +got):\n%s", translator, diff)
				}
			}
		})
	}
}
