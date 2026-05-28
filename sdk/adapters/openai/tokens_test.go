package openai

import (
	"testing"
)

func TestExtractTokens_FullResponse(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"choices": [],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150,
			"prompt_tokens_details": {"cached_tokens": 20},
			"completion_tokens_details": {"reasoning_tokens": 10}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens")
	}
	if tok["input"] != 100 {
		t.Errorf("input: want 100, got %d", tok["input"])
	}
	if tok["output"] != 50 {
		t.Errorf("output: want 50, got %d", tok["output"])
	}
	if tok["cache_read"] != 20 {
		t.Errorf("cache_read: want 20, got %d", tok["cache_read"])
	}
	if tok["reasoning"] != 10 {
		t.Errorf("reasoning: want 10, got %d", tok["reasoning"])
	}
	if _, ok := tok["total"]; ok {
		t.Error("total should not be present in map")
	}
}

func TestExtractTokens_NoUsage(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-abc","choices":[{"delta":{"content":"hello"}}]}`)
	tok := ExtractTokens(body)
	if tok != nil {
		t.Errorf("expected nil for no-usage chunk, got %v", tok)
	}
}

func TestExtractTokens_ZeroUsage(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	tok := ExtractTokens(body)
	if tok != nil {
		t.Errorf("expected nil for zero-usage, got %v", tok)
	}
}

func TestExtractTokens_PartialFields(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":5}}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil when only prompt_tokens present")
	}
	if tok["input"] != 5 {
		t.Errorf("input: want 5, got %d", tok["input"])
	}
	if _, ok := tok["output"]; ok {
		t.Error("output should not be present when zero")
	}
}

func TestExtractTokens_MalformedJSON(t *testing.T) {
	tok := ExtractTokens([]byte(`{bad json`))
	if tok != nil {
		t.Error("expected nil for malformed JSON")
	}
}

func TestExtractTokens_StreamingFinalChunk(t *testing.T) {
	// OpenAI streaming final chunk with include_usage: true
	body := []byte(`{"id":"chatcmpl-xyz","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":15,"total_tokens":45}}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil Tokens from streaming final chunk")
	}
	if tok["input"] != 30 {
		t.Errorf("input: want 30, got %d", tok["input"])
	}
	if tok["output"] != 15 {
		t.Errorf("output: want 15, got %d", tok["output"])
	}
}

func TestExtractTokens_AudioMeters(t *testing.T) {
	body := []byte(`{
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"prompt_tokens_details": {"cached_tokens": 20, "audio_tokens": 30},
			"completion_tokens_details": {"audio_tokens": 12}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil")
	}
	if tok["audio_input"] != 30 {
		t.Errorf("audio_input: want 30, got %d", tok["audio_input"])
	}
	if tok["audio_output"] != 12 {
		t.Errorf("audio_output: want 12, got %d", tok["audio_output"])
	}
	if tok["cache_read"] != 20 {
		t.Errorf("cache_read: want 20, got %d", tok["cache_read"])
	}
}

func TestExtractTokens_SpeculativeDecoding(t *testing.T) {
	body := []byte(`{
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"completion_tokens_details": {
				"accepted_prediction_tokens": 40,
				"rejected_prediction_tokens": 8
			}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil")
	}
	if tok["accepted_prediction"] != 40 {
		t.Errorf("accepted_prediction: want 40, got %d", tok["accepted_prediction"])
	}
	if tok["rejected_prediction"] != 8 {
		t.Errorf("rejected_prediction: want 8, got %d", tok["rejected_prediction"])
	}
}

func TestExtractTokens_AllZeroDetailFieldsStaySilent(t *testing.T) {
	body := []byte(`{
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"prompt_tokens_details": {"cached_tokens": 0, "audio_tokens": 0},
			"completion_tokens_details": {"reasoning_tokens": 0, "audio_tokens": 0,
				"accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0}
		}
	}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil")
	}
	for _, k := range []string{"cache_read", "audio_input", "reasoning", "audio_output",
		"accepted_prediction", "rejected_prediction"} {
		if _, has := tok[k]; has {
			t.Errorf("%s should not appear when zero", k)
		}
	}
}

func TestExtractTokens_NoCacheOrReasoning(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	tok := ExtractTokens(body)
	if tok == nil {
		t.Fatal("expected non-nil")
	}
	if _, ok := tok["cache_read"]; ok {
		t.Error("cache_read should not be present")
	}
	if _, ok := tok["reasoning"]; ok {
		t.Error("reasoning should not be present")
	}
}
