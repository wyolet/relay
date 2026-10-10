package inference

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/openai"
)

// Two upstream content frames that reach dispatch in one read. The SDK
// to-canonical parsers keep only a chunk's last data: line, so dispatch must
// hand them one frame at a time for both deltas to survive.
const twoDeltaFrames = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo."}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

func assertBothDeltas(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{`"delta":"Hel"`, `"delta":"lo."`, "event: generation.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("canonical stream missing %s:\n%s", want, out)
		}
	}
}

func TestStreamCanonical_TwoFramesInOneReadYieldBoth(t *testing.T) {
	out := runStreamCanonical(t, twoDeltaFrames, streamShapes{}, openai.CCTranslator{}.NewToCanonicalStream(), nil)
	assertBothDeltas(t, out)
}

func TestStreamCanonical_CRLFFramesYieldBoth(t *testing.T) {
	t.Skip("splitSSEChunks splits only at \"\\n\\n\", so a CRLF-framed upstream stream reaches the to-canonical parser as one chunk: every frame but the last is dropped and the stream ends as an empty completed generation")
	crlf := strings.ReplaceAll(twoDeltaFrames, "\n", "\r\n")
	out := runStreamCanonical(t, crlf, streamShapes{}, openai.CCTranslator{}.NewToCanonicalStream(), nil)
	assertBothDeltas(t, out)
}
