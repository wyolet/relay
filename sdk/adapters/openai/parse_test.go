package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func withRich(on bool, fn func()) {
	prev := richParsing.Load()
	richParsing.Store(on)
	defer func() { richParsing.Store(prev) }()
	fn()
}

func TestParse_BodyMetadataRichMode(t *testing.T) {
	withRich(true, func() {
		body := `{"model":"gpt-4","metadata":{"customer":"acme"}}`
		cr, err := Parse(context.Background(), []byte(body), http.Header{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cr.Metadata == nil || cr.Metadata["customer"] != "acme" {
			t.Errorf("Metadata = %v, want customer=acme", cr.Metadata)
		}
	})
}

func TestParse_BodyMetadataMinimalMode(t *testing.T) {
	withRich(false, func() {
		body := `{"model":"gpt-4","metadata":{"customer":"acme"}}`
		cr, err := Parse(context.Background(), []byte(body), http.Header{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cr.Metadata != nil {
			t.Errorf("Metadata = %v, want nil in minimal mode", cr.Metadata)
		}
	})
}

func TestParse_MetadataCapsViolations(t *testing.T) {
	cases := []struct {
		name string
		meta string
	}{
		{"17 entries", func() string {
			var sb strings.Builder
			sb.WriteString(`{"model":"gpt-4","metadata":{`)
			for i := 0; i < 17; i++ {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(`"k` + string(rune('a'+i)) + `":"v"`)
			}
			sb.WriteString(`}}`)
			return sb.String()
		}()},
		{"key too long", `{"model":"gpt-4","metadata":{"` + strings.Repeat("k", 65) + `":"v"}}`},
		{"value too long", `{"model":"gpt-4","metadata":{"k":"` + strings.Repeat("v", 257) + `"}}`},
		{"bad key charset", `{"model":"gpt-4","metadata":{"bad key!":"v"}}`},
		{"value contains comma", `{"model":"gpt-4","metadata":{"k":"a,b"}}`},
		{"value contains equals", `{"model":"gpt-4","metadata":{"k":"a=b"}}`},
	}

	withRich(true, func() {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cr, err := Parse(context.Background(), []byte(tc.meta), http.Header{})
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cr.Metadata != nil {
					t.Errorf("Metadata should be nil on caps violation, got %v", cr.Metadata)
				}
			})
		}
	})
}

func TestParse_EmptyMetadataNoExtraMapAlloc(t *testing.T) {
	withRich(true, func() {
		bodyNoMeta := []byte(`{"model":"gpt-4","messages":[]}`)
		bodyWithMeta := []byte(`{"model":"gpt-4","metadata":{"k":"v"},"messages":[]}`)

		allocsNoMeta := testing.AllocsPerRun(100, func() {
			cr, _ := Parse(context.Background(), bodyNoMeta, http.Header{})
			_ = cr
		})
		allocsWithMeta := testing.AllocsPerRun(100, func() {
			cr, _ := Parse(context.Background(), bodyWithMeta, http.Header{})
			_ = cr
		})
		// When metadata is absent, there must be fewer allocations than when present
		// (the map itself is one extra alloc). This proves no map allocation on the
		// empty path.
		if allocsWithMeta <= allocsNoMeta {
			t.Errorf("expected more allocs with metadata (%v) than without (%v)", allocsWithMeta, allocsNoMeta)
		}
	})
}

func TestParse_MalformedJSON(t *testing.T) {
	withRich(true, func() {
		_, err := Parse(context.Background(), []byte("not json"), http.Header{})
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
		status, _, ok := ParseError(err)
		if !ok || status != http.StatusBadRequest {
			t.Errorf("want 400 parseError, got status=%d ok=%v", status, ok)
		}
	})
}

func TestParse_MissingModel(t *testing.T) {
	withRich(true, func() {
		_, err := Parse(context.Background(), []byte(`{"stream":true}`), http.Header{})
		if err == nil {
			t.Fatal("expected error for missing model")
		}
		status, body, ok := ParseError(err)
		if !ok || status != http.StatusBadRequest {
			t.Errorf("want 400 parseError, got status=%d ok=%v", status, ok)
		}
		var env errEnvelope
		if je := json.Unmarshal(body, &env); je != nil {
			t.Fatalf("unmarshal error body: %v", je)
		}
		if env.Error.Code != "missing_model" {
			t.Errorf("code = %q, want missing_model", env.Error.Code)
		}
	})
}

func TestParse_ExtraFieldsPreservedInRaw(t *testing.T) {
	withRich(true, func() {
		body := `{"model":"gpt-4","max_tokens":100,"tools":[]}`
		cr, err := Parse(context.Background(), []byte(body), http.Header{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(cr.Raw) != body {
			t.Errorf("Raw = %s, want %s", cr.Raw, body)
		}
	})
}

func TestParse_MessagesPreservedAsRawMessage(t *testing.T) {
	withRich(true, func() {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
		cr, err := Parse(context.Background(), []byte(body), http.Header{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cr.Messages) != 1 {
			t.Fatalf("Messages len = %d, want 1", len(cr.Messages))
		}
		var m map[string]string
		if err := json.Unmarshal(cr.Messages[0], &m); err != nil {
			t.Fatalf("unmarshal message: %v", err)
		}
		if m["role"] != "user" {
			t.Errorf("role = %q, want user", m["role"])
		}
	})
}

func TestParse_StreamAndUserExtracted(t *testing.T) {
	withRich(false, func() {
		body := `{"model":"gpt-4","stream":true,"user":"alice"}`
		cr, err := Parse(context.Background(), []byte(body), http.Header{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cr.Stream {
			t.Error("Stream should be true")
		}
		if cr.User != "alice" {
			t.Errorf("User = %q, want alice", cr.User)
		}
	})
}
