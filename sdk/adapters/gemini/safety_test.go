package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

const blockedPromptBody = `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH"}]},"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`

func TestParseResponse_BlockedPromptSurfacesContentFilter(t *testing.T) {
	resp, err := tr.ParseResponse([]byte(blockedPromptBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonContentFilter || resp.Status != v1.StatusIncomplete {
		t.Errorf("status/finish = %q/%q, want incomplete/content_filter", resp.Status, resp.FinishReason)
	}
	if !strings.Contains(string(resp.Extensions[extPromptFeedback]), `"blockReason":"SAFETY"`) {
		t.Errorf("prompt feedback not carried: %v", resp.Extensions)
	}
}

func TestParseResponse_SafetyRatingsCarried(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0,"safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"NEGLIGIBLE"}]}]}`
	resp, err := tr.ParseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	ratings := resp.Extensions[extSafetyRatings]
	if !strings.Contains(string(ratings), "HARM_CATEGORY_HATE_SPEECH") {
		t.Fatalf("safety ratings not carried: %v", resp.Extensions)
	}

	out, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"safetyRatings"`) {
		t.Errorf("safety ratings not re-emitted: %s", out)
	}
}

func TestSerializeResponse_BlockedPromptRoundTrip(t *testing.T) {
	resp, err := tr.ParseResponse([]byte(blockedPromptBody))
	if err != nil {
		t.Fatal(err)
	}
	out, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["candidates"]; ok {
		t.Errorf("blocked prompt must carry no candidate: %s", out)
	}
	if !strings.Contains(string(m["promptFeedback"]), "SAFETY") {
		t.Errorf("promptFeedback lost: %s", out)
	}
}

func TestStreamGeminiToCanonical_BlockedPromptTerminates(t *testing.T) {
	fn := tr.NewToCanonicalStream()
	out, err := fn([]byte("data: " + blockedPromptBody + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "event: "+v1.EventGenerationCompleted) {
		t.Fatalf("blocked prompt stream has no terminal event: %s", s)
	}
	if !strings.Contains(s, `"finish_reason":"content_filter"`) {
		t.Errorf("blocked prompt stream must end in content_filter: %s", s)
	}
}
