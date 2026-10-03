package anthropic

import (
	"bytes"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// The idle frame must be byte-identical to the ping the stream already emits at open: one dialect for "still here", not two.
func TestKeepAliveFrame_MatchesStreamOpenPing(t *testing.T) {
	frame := v1.KeepAliveFrameFor(AnthropicTranslator{})

	opened, err := (&canonicalToAnthropicStream{}).handleGenerationCreated(
		[]byte(`{"id":"msg_001","model":"claude-test"}`))
	if err != nil {
		t.Fatalf("generation.created: %v", err)
	}
	idx := bytes.Index(opened, []byte("event: ping"))
	if idx < 0 {
		t.Fatalf("stream open emitted no ping: %q", opened)
	}
	if got := opened[idx:]; !bytes.Equal(got, frame) {
		t.Fatalf("keepalive frame %q != stream-open ping %q", frame, got)
	}
	if !bytes.Equal(frame, []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")) {
		t.Fatalf("unexpected ping wire bytes: %q", frame)
	}
}
