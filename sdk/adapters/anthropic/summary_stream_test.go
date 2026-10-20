package anthropic

import (
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// translateAll hides a translator's v1.SummaryStream so summaries built on it
// translate every frame.
type translateAll struct{ v1.Translator }

func TestAnthropicSummaryFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"text delta", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"message_stop\"}}", false},
		{"delta without space", "event:content_block_delta\ndata:{\"type\":\"content_block_delta\"}", false},
		{"delta CRLF", "event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\"}", false},
		{"ping", "event: ping\ndata: {\"type\": \"ping\"}", false},
		{"no event name", "data: {\"type\":\"message_stop\"}", false},
		{"message_start", "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":3,\"service_tier\":\"priority\"}}}", true},
		{"message_delta", "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}", true},
		{"message_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}", true},
		{"message_stop CRLF", "event: message_stop\r\ndata: {\"type\":\"message_stop\"}", true},
		{"error", "event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"x\"}}", true},
		{"block start", "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}", true},
		{"block stop", "event: content_block_stop\ndata: {\"index\":0}", true},
		{"unknown event", "event: message_metadata\ndata: {}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, affects := AnthropicTranslator{}.NewSummaryStream()
			if got := affects([]byte(tc.frame)); got != tc.want {
				t.Errorf("affectsSummary = %v, want %v", got, tc.want)
			}
		})
	}
}

// The structured-output tool pins the stop reason at its content_block_stop,
// which the predicate must therefore let through.
func TestAnthropicSummaryFrameStructuredOutput(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":5}}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"" + structuredOutputToolName + "\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	}
	body := []byte(strings.Join(frames, "\n\n") + "\n\n")
	filtered, _ := v1.ExtractSummary(AnthropicTranslator{}, body)
	all, _ := v1.ExtractSummary(translateAll{AnthropicTranslator{}}, body)
	if !reflect.DeepEqual(filtered, all) {
		t.Fatalf("filtered %+v, unfiltered %+v", filtered, all)
	}
	if filtered.FinishReason != v1.FinishReasonStop {
		t.Errorf("finish = %q, want stop", filtered.FinishReason)
	}
}
