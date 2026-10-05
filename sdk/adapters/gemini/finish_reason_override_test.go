package gemini_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/gemini"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// A function-call part must not promote a safety or truncation finish reason
// to tool_calls. Only a neutral STOP (or empty) finish reason should yield
// tool_calls when there is a function call in the output (rule 11).

func TestGeminiParse_SafetyWithFunctionCall_PreservesContentFilter(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"x"}}}]},"finishReason":"SAFETY","index":0,
	  "safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true}]}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"g"}`)
	resp, err := gemini.GeminiTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason == v1.FinishReasonToolCalls {
		t.Fatalf("SAFETY with a functionCall part must not produce finish_reason=tool_calls; got status=%q finish=%q", resp.Status, resp.FinishReason)
	}
	if resp.FinishReason != v1.FinishReasonContentFilter {
		t.Fatalf("SAFETY finish reason must map to content_filter; got %q", resp.FinishReason)
	}
	if resp.Status == v1.StatusCompleted {
		t.Fatalf("SAFETY must not produce status=completed; got %q", resp.Status)
	}
}

func TestGeminiParse_MaxTokensWithFunctionCall_PreservesLength(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"x"}}}]},"finishReason":"MAX_TOKENS","index":0}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"g"}`)
	resp, err := gemini.GeminiTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason == v1.FinishReasonToolCalls {
		t.Fatalf("MAX_TOKENS with a functionCall part must not produce finish_reason=tool_calls; got status=%q finish=%q", resp.Status, resp.FinishReason)
	}
	if resp.FinishReason != v1.FinishReasonLength {
		t.Fatalf("MAX_TOKENS finish reason must map to length; got %q", resp.FinishReason)
	}
	if resp.Status != v1.StatusIncomplete {
		t.Fatalf("MAX_TOKENS must produce status=incomplete; got %q", resp.Status)
	}
}

// STOP + functionCall must still yield tool_calls (the normal tool-call path).
func TestGeminiParse_StopWithFunctionCall_YieldsToolCalls(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"x"}}}]},"finishReason":"STOP","index":0}],
	  "usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"g"}`)
	resp, err := gemini.GeminiTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonToolCalls {
		t.Fatalf("STOP+functionCall must produce finish_reason=tool_calls; got %q", resp.FinishReason)
	}
}

// SAFETY alone (no functionCall) must still yield content_filter (control).
func TestGeminiParse_SafetyAlone_YieldsContentFilter(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"SAFETY","index":0}],"modelVersion":"g"}`)
	resp, err := gemini.GeminiTranslator{}.ParseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != v1.FinishReasonContentFilter {
		t.Fatalf("SAFETY alone must produce content_filter; got %q", resp.FinishReason)
	}
}

// Stream: early functionCall frame then terminal SAFETY frame must preserve SAFETY.
func TestGeminiStream_SafetyAfterFunctionCall_PreservesContentFilter(t *testing.T) {
	toCanon := gemini.GeminiTranslator{}.NewToCanonicalStream()
	var canon []byte
	for _, frame := range [][]byte{
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"x"}}}]},"index":0}],"modelVersion":"g"}` + "\n\n"),
		[]byte(`data: {"candidates":[{"finishReason":"SAFETY","index":0,"safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH","blocked":true}]}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"g"}` + "\n\n"),
	} {
		out, err := toCanon(frame)
		if err != nil {
			t.Fatal(err)
		}
		canon = append(canon, out...)
	}

	if strings.Contains(string(canon), `"finish_reason":"tool_calls"`) {
		t.Fatalf("SAFETY terminal after a functionCall frame must not produce finish_reason=tool_calls in canonical stream:\n%s", canon)
	}
	if !strings.Contains(string(canon), `"finish_reason":"content_filter"`) {
		t.Fatalf("SAFETY terminal must produce finish_reason=content_filter in canonical stream:\n%s", canon)
	}
}

// Stream: early functionCall frame then terminal STOP frame must still yield tool_calls.
func TestGeminiStream_StopAfterFunctionCall_YieldsToolCalls(t *testing.T) {
	toCanon := gemini.GeminiTranslator{}.NewToCanonicalStream()
	var canon []byte
	for _, frame := range [][]byte{
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"x"}}}]},"index":0}],"modelVersion":"g"}` + "\n\n"),
		[]byte(`data: {"candidates":[{"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"g"}` + "\n\n"),
	} {
		out, err := toCanon(frame)
		if err != nil {
			t.Fatal(err)
		}
		canon = append(canon, out...)
	}

	if !strings.Contains(string(canon), `"finish_reason":"tool_calls"`) {
		t.Fatalf("STOP terminal after a functionCall frame must produce finish_reason=tool_calls:\n%s", canon)
	}
}
