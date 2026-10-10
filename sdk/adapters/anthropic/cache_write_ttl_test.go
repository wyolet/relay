package anthropic

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Anthropic reports the cache-write total and, nested, its split by cache TTL; 1-hour writes are billed at a higher rate than 5-minute ones.
const ttlSplitBody = `{"id":"msg_ttl","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500},"output_tokens":300}}`

const ttlSplitStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_ttl","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500},"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":300}}

event: message_stop
data: {"type":"message_stop"}

`

var ttlSplitTokens = usage.Tokens{"input": 1000, "cache_creation": 4000, "cache_creation_1h": 1500, "output": 300}

func TestExtractTokens_CacheWriteTTLSplit(t *testing.T) {
	for name, body := range map[string]string{"buffered": ttlSplitBody, "stream": ttlSplitStream} {
		t.Run(name, func(t *testing.T) {
			if got := ExtractTokens([]byte(body)); !maps.Equal(got, ttlSplitTokens) {
				t.Errorf("ExtractTokens = %v, want %v", got, ttlSplitTokens)
			}
		})
	}
}

func TestParseResponse_CacheWriteTTLSplit(t *testing.T) {
	resp, err := (AnthropicTranslator{}).ParseResponse([]byte(ttlSplitBody))
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(resp.Usage, ttlSplitTokens) {
		t.Errorf("Usage = %v, want %v", resp.Usage, ttlSplitTokens)
	}
}

func TestToCanonicalStream_CacheWriteTTLSplit(t *testing.T) {
	translate := (AnthropicTranslator{}).NewToCanonicalStream()
	var completed *v1.GenerationCompletedEvent
	for _, chunk := range splitFrames([]byte(ttlSplitStream)) {
		out, err := translate(chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range splitFrames(out) {
			if ev, data, ok := v1.ParseSSEChunk(frame); ok && ev == v1.EventGenerationCompleted {
				completed = &v1.GenerationCompletedEvent{}
				if err := json.Unmarshal(data, completed); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if completed == nil {
		t.Fatal("no generation.completed event")
	}
	if !maps.Equal(completed.Usage, ttlSplitTokens) {
		t.Errorf("Usage = %v, want %v", completed.Usage, ttlSplitTokens)
	}
}

func TestSerializeResponse_CacheWriteTTLSplitRoundTrip(t *testing.T) {
	tr := AnthropicTranslator{}
	resp, err := tr.ParseResponse([]byte(ttlSplitBody))
	if err != nil {
		t.Fatal(err)
	}
	body, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Usage struct {
			CacheCreation map[string]int64 `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	wantSplit := map[string]int64{"ephemeral_5m_input_tokens": 2500, "ephemeral_1h_input_tokens": 1500}
	if !maps.Equal(wire.Usage.CacheCreation, wantSplit) {
		t.Errorf("usage.cache_creation = %v, want %v", wire.Usage.CacheCreation, wantSplit)
	}
	again, err := tr.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(again.Usage, ttlSplitTokens) {
		t.Errorf("round-trip Usage = %v, want %v", again.Usage, ttlSplitTokens)
	}
	if got := ExtractTokens(body); !maps.Equal(got, ttlSplitTokens) {
		t.Errorf("ExtractTokens on serialized body = %v, want %v", got, ttlSplitTokens)
	}
}

// Without a reported 1-hour share the TTL split is unknown, so none is written.
func TestSerializeResponse_NoTTLSplitWithout1h(t *testing.T) {
	resp := &v1.Response{Usage: usage.Tokens{"input": 10, "cache_creation": 4000, "output": 5}}
	body, err := (AnthropicTranslator{}).SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := decodeMap(t, body)["usage"].(map[string]any)
	if _, ok := u["cache_creation"]; ok {
		t.Errorf("usage.cache_creation written without a 1h share: %v", u)
	}
	if u["cache_creation_input_tokens"] != float64(4000) {
		t.Errorf("cache_creation_input_tokens = %v, want 4000", u["cache_creation_input_tokens"])
	}
}
