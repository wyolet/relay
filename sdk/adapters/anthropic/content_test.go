package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertContentToOpenAI_PlainString(t *testing.T) {
	in := json.RawMessage(`"hello"`)
	got := convertContentToOpenAI(in)
	if string(got) != `"hello"` {
		t.Fatalf("string content should pass through; got %q", got)
	}
}

func TestConvertContentToOpenAI_AnthropicImageURL(t *testing.T) {
	in := json.RawMessage(`[
        {"type":"text","text":"look at this"},
        {"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}
    ]`)
	got := convertContentToOpenAI(in)
	if !strings.Contains(string(got), `"type":"image_url"`) {
		t.Fatalf("expected image_url part; got %s", got)
	}
	if !strings.Contains(string(got), `"url":"https://example.com/cat.png"`) {
		t.Fatalf("expected url preserved; got %s", got)
	}
}

func TestConvertContentToOpenAI_AnthropicImageBase64(t *testing.T) {
	in := json.RawMessage(`[
        {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAA="}}
    ]`)
	got := convertContentToOpenAI(in)
	want := `"url":"data:image/jpeg;base64,AAA="`
	if !strings.Contains(string(got), want) {
		t.Fatalf("expected data URL %q in %s", want, got)
	}
}

func TestConvertContentFromOpenAI_OpenAIImageHTTPS(t *testing.T) {
	in := json.RawMessage(`[
        {"type":"text","text":"caption this"},
        {"type":"image_url","image_url":{"url":"https://example.com/dog.png"}}
    ]`)
	got := convertContentFromOpenAI(in)
	if !strings.Contains(string(got), `"type":"image"`) {
		t.Fatalf("expected anthropic image block; got %s", got)
	}
	if !strings.Contains(string(got), `"type":"url"`) || !strings.Contains(string(got), `"url":"https://example.com/dog.png"`) {
		t.Fatalf("expected url source; got %s", got)
	}
}

func TestConvertContentFromOpenAI_DataURL(t *testing.T) {
	in := json.RawMessage(`[
        {"type":"image_url","image_url":{"url":"data:image/png;base64,BBB="}}
    ]`)
	got := convertContentFromOpenAI(in)
	if !strings.Contains(string(got), `"type":"base64"`) {
		t.Fatalf("expected base64 source; got %s", got)
	}
	if !strings.Contains(string(got), `"media_type":"image/png"`) {
		t.Fatalf("expected media_type extracted; got %s", got)
	}
	if !strings.Contains(string(got), `"data":"BBB="`) {
		t.Fatalf("expected raw data extracted; got %s", got)
	}
}

func TestConvertContentFromOpenAI_RoundTripURL(t *testing.T) {
	original := json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://x.com/y.png"}}]`)
	// openai → anthropic → openai
	anthShape := convertContentFromOpenAI(original)
	backToOpenAI := convertContentToOpenAI(anthShape)
	if !strings.Contains(string(backToOpenAI), `"url":"https://x.com/y.png"`) {
		t.Fatalf("URL did not survive round-trip: %s", backToOpenAI)
	}
}

func TestConvertContentFromOpenAI_RoundTripBase64(t *testing.T) {
	original := json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,ZZZ="}}]`)
	anthShape := convertContentFromOpenAI(original)
	backToOpenAI := convertContentToOpenAI(anthShape)
	if !strings.Contains(string(backToOpenAI), `"data:image/jpeg;base64,ZZZ="`) {
		t.Fatalf("data URL did not survive round-trip: %s", backToOpenAI)
	}
}
