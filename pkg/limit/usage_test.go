package limit

import "testing"

func TestParseTokens_NonStreamingBody(t *testing.T) {
	b := []byte(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	n, ok := ParseTokens(b)
	if !ok || n != 30 {
		t.Fatalf("want (30,true), got (%d,%v)", n, ok)
	}
}

func TestParseTokens_StreamingDataPrefix(t *testing.T) {
	b := []byte("data: {\"usage\":{\"total_tokens\":42}}\n\n")
	n, ok := ParseTokens(b)
	if !ok || n != 42 {
		t.Fatalf("want (42,true), got (%d,%v)", n, ok)
	}
}

func TestParseTokens_StreamingDataDONE(t *testing.T) {
	b := []byte("data: [DONE]")
	n, ok := ParseTokens(b)
	if ok || n != 0 {
		t.Fatalf("want (0,false), got (%d,%v)", n, ok)
	}
}

func TestParseTokens_NoUsageField(t *testing.T) {
	b := []byte(`{"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`)
	n, ok := ParseTokens(b)
	if ok || n != 0 {
		t.Fatalf("want (0,false), got (%d,%v)", n, ok)
	}
}

func TestParseTokens_MalformedJSON(t *testing.T) {
	b := []byte(`not-json!!!`)
	n, ok := ParseTokens(b)
	if ok || n != 0 {
		t.Fatalf("want (0,false), got (%d,%v)", n, ok)
	}
}

func TestParseTokens_EmptyChunk(t *testing.T) {
	for _, b := range [][]byte{[]byte(""), []byte("\n\n"), []byte("  ")} {
		n, ok := ParseTokens(b)
		if ok || n != 0 {
			t.Fatalf("want (0,false) for %q, got (%d,%v)", b, n, ok)
		}
	}
}

func TestParseTokens_SumPromptAndCompletion(t *testing.T) {
	b := []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":8}}`)
	n, ok := ParseTokens(b)
	if !ok || n != 13 {
		t.Fatalf("want (13,true), got (%d,%v)", n, ok)
	}
}
