package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func withRich(on bool, fn func()) {
	prev := richParsing.Load()
	richParsing.Store(on)
	defer func() { richParsing.Store(prev) }()
	fn()
}

func TestParse(t *testing.T) {
	body := `{"model":"gpt-4","stream":true,"user":"alice","metadata":{"azAZ09_.-":"acme co ~1"},"messages":[{"role":"user","content":"hi"}],"tools":[]}`
	cases := []struct {
		rich bool
		want ChatRequest
	}{
		{true, ChatRequest{Model: "gpt-4", Stream: true, User: "alice", Metadata: map[string]string{"azAZ09_.-": "acme co ~1"},
			Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}, Raw: json.RawMessage(body)}},
		{false, ChatRequest{Model: "gpt-4", Stream: true, User: "alice", Raw: json.RawMessage(body)}},
	}
	for _, tc := range cases {
		withRich(tc.rich, func() {
			got, err := Parse(context.Background(), []byte(body), http.Header{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("rich=%v: got %+v\nwant %+v", tc.rich, *got, tc.want)
			}
		})
	}
}

// Metadata breaking a cap is dropped whole; the request still parses.
func TestParse_MetadataCapsViolations(t *testing.T) {
	var many []string
	for i := range 17 {
		many = append(many, `"k`+string(rune('a'+i))+`":"v"`)
	}
	cases := map[string]string{
		"17 entries":            "{" + strings.Join(many, ",") + "}",
		"key too long":          `{"` + strings.Repeat("k", 65) + `":"v"}`,
		"value too long":        `{"k":"` + strings.Repeat("v", 257) + `"}`,
		"bad key charset":       `{"bad key!":"v"}`,
		"empty key":             `{"":"v"}`,
		"key with a slash":      `{"a/b":"v"}`,
		"key with a colon":      `{"a:b":"v"}`,
		"key with a bracket":    `{"a[b":"v"}`,
		"key with a backtick":   "{\"a`b\":\"v\"}",
		"key with a brace":      `{"a{b":"v"}`,
		"value not ASCII":       `{"k":"é"}`,
		"empty":                 `{}`,
		"value contains comma":  `{"k":"a,b"}`,
		"value contains equals": `{"k":"a=b"}`,
		"value control char":    `{"k":"a\nb"}`,
	}
	withRich(true, func() {
		for name, meta := range cases {
			cr, err := Parse(context.Background(), []byte(`{"model":"gpt-4","metadata":`+meta+`}`), http.Header{})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if cr.Metadata != nil {
				t.Errorf("%s: Metadata = %v, want nil", name, cr.Metadata)
			}
		}
		atCaps := "{" + strings.Join(many[:15], ",") + `,"` + strings.Repeat("k", 64) + `":"` + strings.Repeat("v", 256) + `"}`
		if cr, err := Parse(context.Background(), []byte(`{"model":"gpt-4","metadata":`+atCaps+`}`), http.Header{}); err != nil || len(cr.Metadata) != 16 {
			t.Errorf("metadata at the caps: kept %d entries (%v), want 16", len(cr.Metadata), err)
		}
	})
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"not json":        `{"error":{"message":"request body is not valid JSON","type":"invalid_request_error","code":"invalid_body"}}`,
		`{"stream":true}`: `{"error":{"message":"model is required","type":"invalid_request_error","code":"missing_model"}}`,
	}
	for body, want := range cases {
		_, err := Parse(context.Background(), []byte(body), http.Header{})
		status, got, ok := ParseError(err)
		if !ok || status != http.StatusBadRequest {
			t.Errorf("%s: status %d (parse error %v), want 400", body, status, ok)
		}
		assertJSON(t, got, want)
		assertJSON(t, []byte(err.Error()), want)
	}
	if _, _, ok := ParseError(errors.New("other")); ok {
		t.Error("ParseError accepted an error Parse did not return")
	}
}

func TestSetRichParsing(t *testing.T) {
	if !RichParsing() {
		t.Error("rich parsing is off by default")
	}
	SetRichParsing(false)
	defer SetRichParsing(true)
	if RichParsing() {
		t.Error("rich parsing still on after SetRichParsing(false)")
	}
}

func TestChatRequestContext(t *testing.T) {
	cr := &ChatRequest{Model: "gpt-4"}
	if got, ok := ChatRequestFromContext(ContextWithChatRequest(context.Background(), cr)); !ok || got != cr {
		t.Errorf("stored request: got %p, %v", got, ok)
	}
	if _, ok := ChatRequestFromContext(context.Background()); ok {
		t.Error("empty context: want ok=false")
	}
	if _, ok := ChatRequestFromContext(ContextWithChatRequest(context.Background(), nil)); ok {
		t.Error("nil request: want ok=false")
	}
}
