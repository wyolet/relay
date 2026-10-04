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

// Gemini has no refusal finishReason; SAFETY is the closest one a client
// treats as a blocked completion. The refusal text stays the candidate
// content. Re-parsed it reads as content_filter — still not a clean stop.
func TestSerializeResponse_RefusalIsNotStop(t *testing.T) {
	resp := &v1.Response{
		Status:       v1.StatusCompleted,
		FinishReason: v1.FinishReasonRefusal,
		Output: []v1.Item{&v1.Message{
			Role:    v1.RoleAssistant,
			Content: []v1.Part{&v1.OutputTextPart{Text: "I can't help with that."}},
		}},
	}
	out, err := tr.SerializeResponse(resp, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"finishReason":"SAFETY"`) {
		t.Errorf("refusal must not serialize as STOP: %s", s)
	}
	if !strings.Contains(s, "I can't help with that.") {
		t.Errorf("refusal text must stay the candidate content: %s", s)
	}

	back, err := tr.ParseResponse(out)
	if err != nil {
		t.Fatal(err)
	}
	if back.FinishReason != v1.FinishReasonContentFilter {
		t.Errorf("round-trip finish_reason = %q, want content_filter", back.FinishReason)
	}
}

func TestStreamCanonicalToGemini_RefusalIsNotStop(t *testing.T) {
	fn := tr.NewFromCanonicalStream()
	var out []byte
	for _, f := range []v1.SSEFrame{
		{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"r1","model":"m"}`)},
		{Event: v1.EventItemStarted, Data: []byte(`{"item_id":"msg_0","item_type":"message"}`)},
		{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"msg_0","kind":"text","delta":"no."}`)},
		{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"r1","status":"completed","finish_reason":"refusal"}`)},
	} {
		b, err := fn(f.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	if !strings.Contains(string(out), `"finishReason":"SAFETY"`) {
		t.Errorf("streamed refusal must not end as STOP: %s", out)
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
