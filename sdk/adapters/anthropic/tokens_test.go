package anthropic

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/usage"
)

// cache_creation and its ttl split appear only in message_start; message_delta carries the final output count and repeats input and cache_read.
const usageStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"output_tokens":1,"cache_creation_input_tokens":4000,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500}}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"cache_read_input_tokens":2048,"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestExtractTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want usage.Tokens
	}{
		{
			"buffered response",
			`{"id":"msg_1","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":25,"cache_read_input_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":20},"server_tool_use":{"input_tokens":3,"output_tokens":2}}}`,
			usage.Tokens{"input": 100, "output": 50, "cache_creation": 25, "cache_creation_1h": 20, "cache_read": 10, "server_tool_use_input": 3, "server_tool_use_output": 2},
		},
		{"zero counters are left out", ` {"usage":{"input_tokens":7,"output_tokens":0}}`, usage.Tokens{"input": 7}},
		{
			"message_start object",
			`{"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":5,"cache_read_input_tokens":9,"cache_creation":{"ephemeral_1h_input_tokens":4}}}}`,
			usage.Tokens{"input": 25, "output": 1, "cache_creation": 5, "cache_creation_1h": 4, "cache_read": 9},
		},
		{"message_delta object", `{"type":"message_delta","usage":{"output_tokens":15}}`, usage.Tokens{"output": 15}},
		{"whole stream", usageStream, usage.Tokens{"input": 12, "output": 5, "cache_creation": 4000, "cache_creation_1h": 1500, "cache_read": 2048}},
		{
			"data split over two lines",
			"event: message_start\ndata: {\"type\":\"message_start\",\ndata: \"message\":{\"usage\":{\"input_tokens\":3,\"cache_creation_input_tokens\":7}}}\n\nevent: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n",
			usage.Tokens{"input": 3, "cache_creation": 7, "output": 5},
		},
		{"stream without usage", "event: ping\ndata: {\"type\":\"ping\"}\n\ndata: [DONE]\n\n", nil},
		{"object without usage", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, nil},
		{"all-zero usage", `{"usage":{"input_tokens":0,"output_tokens":0}}`, nil},
		{"message without usage", `{"message":{"id":"msg_1"}}`, nil},
		{"message with all-zero usage", `{"message":{"usage":{"input_tokens":0}}}`, nil},
		{"malformed object", `{not json`, nil},
		{"blank body", " \n", nil},
	} {
		if diff := cmp.Diff(tc.want, ExtractTokens([]byte(tc.body))); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}
