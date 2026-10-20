package anthropic

import (
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestServiceTier_ReachesSummary(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"service_tier":"priority"}}`
	s, err := v1.ExtractSummary(AnthropicTranslator{}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if s.ServiceTier != "priority" {
		t.Fatalf("service tier = %q, want %q", s.ServiceTier, "priority")
	}
}

func TestServiceTier_StreamMatchesBuffered(t *testing.T) {
	buffered := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"service_tier":"priority"}}`
	startWith := `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":1,"service_tier":"priority"}}}`
	startWithout := `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`
	deltaWith := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"service_tier":"priority"}}`
	deltaWithout := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`
	cases := map[string]struct{ start, delta string }{
		"tier on message_start": {startWith, deltaWithout},
		"tier on message_delta": {startWithout, deltaWith},
	}
	want, err := v1.ExtractSummary(AnthropicTranslator{}, []byte(buffered))
	if err != nil || want.ServiceTier != "priority" {
		t.Fatalf("buffered: service tier %q, err %v", want.ServiceTier, err)
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stream := "event: message_start\ndata: " + tc.start + "\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: " + tc.delta + "\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			got, err := v1.ExtractSummary(AnthropicTranslator{}, []byte(stream))
			if err != nil {
				t.Fatal(err)
			}
			if got.ServiceTier != want.ServiceTier {
				t.Fatalf("stream service tier = %q, buffered = %q", got.ServiceTier, want.ServiceTier)
			}
		})
	}
}
