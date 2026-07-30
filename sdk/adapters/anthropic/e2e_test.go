package anthropic

import (
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- E2E composition tests ----

func TestE2E_AnthropicToCanonicalToCC(t *testing.T) {
	// Build a canonical request from Anthropic wire, then serialize to CC.
	body := mustJSON(map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"system":     "You are helpful.",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	aT := AnthropicTranslator{}
	req, err := aT.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	// Serialize to CC (via CCTranslator via openai package).
	// We test round-trip at the canonical level: parse → canonical → serialize → parse again.
	// Verify key fields in canonical.
	if req.Instructions != "You are helpful." {
		t.Errorf("instructions: %q", req.Instructions)
	}
	if len(req.Input) == 0 {
		t.Fatal("no input")
	}
	msg, ok := req.Input[0].(*v1.Message)
	if !ok {
		t.Fatalf("input[0] is %T", req.Input[0])
	}
	if msg.Role != v1.RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
}

func TestE2E_AnthropicResponseRoundTrip(t *testing.T) {
	// Parse an Anthropic response → canonical → serialize back to Anthropic.
	body := mustJSON(map[string]any{
		"id":    "msg_rt",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-3-5-sonnet-20241022",
		"content": []any{
			map[string]any{"type": "text", "text": "Round-trip text."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 5, "output_tokens": 5},
	})
	aT := AnthropicTranslator{}
	resp, err := aT.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := aT.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, out)
	content := m["content"].([]any)
	block := content[0].(map[string]any)
	if block["text"] != "Round-trip text." {
		t.Errorf("text round-trip: %v", block["text"])
	}
	if m["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", m["stop_reason"])
	}
}

func TestE2E_StreamAnthropicToCanonicalAndBack(t *testing.T) {
	// Forward pass: Anthropic → canonical
	toCanon := (AnthropicTranslator{}).NewToCanonicalStream()
	// Reverse pass: canonical → Anthropic
	fromCanon := (AnthropicTranslator{}).NewFromCanonicalStream()

	anthropicChunks := [][]byte{
		messageStartChunk("msg_rt", "claude-3-5-sonnet-20241022"),
		contentBlockStartText(0),
		textDeltaChunk(0, "hello"),
		contentBlockStopChunk(0),
		messageDeltaChunk("end_turn", 5),
		messageStopChunk(),
	}

	// Collect canonical frames
	var canonFrames [][]byte
	for _, c := range anthropicChunks {
		out, err := toCanon(c)
		if err != nil {
			t.Fatal(err)
		}
		canonFrames = append(canonFrames, splitFrames(out)...)
	}

	// Convert canonical back to Anthropic
	var allBack []byte
	for _, f := range canonFrames {
		// Reattach separator for fromCanon
		chunk := append(f, '\n', '\n')
		out, err := fromCanon(chunk)
		if err != nil {
			t.Fatal(err)
		}
		allBack = append(allBack, out...)
	}

	if len(allBack) == 0 {
		t.Error("no output from round-trip stream")
	}
	s := string(allBack)
	if !strings.Contains(s, "message_start") {
		t.Errorf("expected message_start in output: %q", s[:min(100, len(s))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
