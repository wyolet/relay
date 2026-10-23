package adapters_test

// These pin the encoding/json behaviours relay's decoders and encoders rely
// on. They hold on the classic engine and on GOEXPERIMENT=jsonv2 alike, so a
// Go upgrade that changes one of them fails here before it reaches a body.

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

type probe struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
}

func TestJSONDuplicateKeysLastWins(t *testing.T) {
	for _, in := range []string{
		`{"model":"a","model":"b"}`,
		`{"model":"a","MODEL":"b"}`,
		`{"Model":"a","model":"b"}`,
	} {
		var p probe
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p.Model != "b" {
			t.Errorf("%s: model = %q, want the last duplicate %q", in, p.Model, "b")
		}
	}

	var m map[string]string
	if err := json.Unmarshal([]byte(`{"model":"a","model":"b"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "b" || len(m) != 1 {
		t.Errorf("map = %v, want the last duplicate only", m)
	}
}

func TestJSONFieldNamesMatchCaseInsensitively(t *testing.T) {
	for in, want := range map[string]probe{
		`{"MODEL":"x","Max_Tokens":5}`: {Model: "x", MaxTokens: 5},
		// An escaped key is unescaped before matching; U+212A folds to k.
		"{\"\\u006dodel\":\"x\",\"\\u212aey\":1}": {Model: "x"},
		// Case folds; delimiters do not.
		`{"max-tokens":5,"maxtokens":6}`: {},
	} {
		var p probe
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p != want {
			t.Errorf("%s: got %+v, want %+v", in, p, want)
		}
	}
}

func TestJSONMarshalEscapesHTML(t *testing.T) {
	in := struct {
		S string
		R json.RawMessage
	}{S: "<a>&\U00002028", R: json.RawMessage(`{"t": "<b>"}`)}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"S\":\"\\u003ca\\u003e\\u0026\\u2028\",\"R\":{\"t\":\"\\u003cb\\u003e\"}}"; string(out) != want {
		t.Errorf("Marshal = %s, want %s", out, want)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		t.Fatal(err)
	}
	if want := "{\"S\":\"<a>&\\u2028\",\"R\":{\"t\":\"<b>\"}}\n"; buf.String() != want {
		t.Errorf("Encode without HTML escaping = %s, want %s", buf.String(), want)
	}
}

func TestJSONRawMessage(t *testing.T) {
	buf := []byte(`{"x": {"a": 1,  "b": [2]}, "n": null}`)
	var v struct {
		X json.RawMessage `json:"x"`
		N json.RawMessage `json:"n"`
		M json.RawMessage `json:"m"`
	}
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] = ' '
	}
	if string(v.X) != `{"a": 1,  "b": [2]}` {
		t.Errorf("RawMessage = %q, want the value's bytes verbatim, copied out of the input", v.X)
	}
	if string(v.N) != "null" || v.M != nil {
		t.Errorf("null member = %q, absent member = %q; want \"null\" and nil", v.N, v.M)
	}

	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"x":{"a":1,"b":[2]},"n":null,"m":null}`; string(out) != want {
		t.Errorf("Marshal = %s, want compacted values and nil as null: %s", out, want)
	}
	if _, err := json.Marshal(json.RawMessage(`{"a":`)); err == nil {
		t.Error("Marshal of an invalid RawMessage succeeded")
	}
}

func TestJSONInvalidUTF8(t *testing.T) {
	for in, want := range map[string]string{
		"\"a\xffb\"":         "a\U0000FFFDb",
		"\"\\ud800x\"":       "\U0000FFFDx",
		"\"\\ud83d\\ude00\"": "\U0001F600",
		"\"\xed\xa0\x80\"":   "\U0000FFFD\U0000FFFD\U0000FFFD",
	} {
		var s string
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if s != want {
			t.Errorf("%q decoded to %q, want %q", in, s, want)
		}
	}
	var p probe
	if err := json.Unmarshal([]byte("{\"model\":\"a\xffb\"}"), &p); err != nil || p.Model != "a\U0000FFFDb" {
		t.Errorf("model = %q, err = %v; want the invalid byte replaced", p.Model, err)
	}

	// The classic engine writes the replacement as an escape sequence, the v2
	// engine as the raw U+FFFD character; both decode to the same string.
	out, err := json.Marshal("a\xffb")
	if err != nil {
		t.Fatal(err)
	}
	var back string
	if err := json.Unmarshal(out, &back); err != nil || !utf8.Valid(out) || back != "a\U0000FFFDb" {
		t.Errorf("Marshal = %q (decodes to %q, err %v), want valid UTF-8 holding the replacement", out, back, err)
	}

	// A RawMessage is not re-encoded: an invalid byte inside it passes through.
	raw := json.RawMessage("{\"k\":\"a\xffb\"}")
	if out, err := json.Marshal(raw); err != nil || !bytes.Equal(out, raw) {
		t.Errorf("Marshal(RawMessage) = %q, %v; want the bytes unchanged", out, err)
	}
}

func TestJSONErrorTypes(t *testing.T) {
	var syntaxErr *json.SyntaxError
	for _, in := range []string{`{"model":`, `{"model":"a"} x`, `{"model" "a"}`, ``} {
		if err := json.Unmarshal([]byte(in), new(probe)); !errors.As(err, &syntaxErr) {
			t.Errorf("%q: error %v (%T), want *json.SyntaxError", in, err, err)
		}
	}
	var typeErr *json.UnmarshalTypeError
	for _, in := range []string{`{"model":1}`, `{"max_tokens":1.5}`, `{"max_tokens":"1"}`, `[]`} {
		if err := json.Unmarshal([]byte(in), new(probe)); !errors.As(err, &typeErr) {
			t.Errorf("%q: error %v (%T), want *json.UnmarshalTypeError", in, err, err)
		}
	}
}

func TestJSONNullAbsentAndOmitempty(t *testing.T) {
	p := probe{Model: "keep", MaxTokens: 3}
	if err := json.Unmarshal([]byte(`{"model":null,"max_tokens":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if p != (probe{Model: "keep", MaxTokens: 3}) {
		t.Errorf("null members changed the fields: %+v", p)
	}

	type inner struct{ A int }
	out, err := json.Marshal(struct {
		Nil     []int          `json:"nil"`
		Empty   []int          `json:"empty"`
		OmitNil []int          `json:"omit_nil,omitempty"`
		OmitMap map[string]int `json:"omit_map,omitempty"`
		OmitStr string         `json:"omit_str,omitempty"`
		OmitPtr *int           `json:"omit_ptr,omitempty"`
		Zero    inner          `json:"zero,omitempty"`
		Sorted  map[string]int `json:"sorted"`
	}{Empty: []int{}, OmitMap: map[string]int{}, Sorted: map[string]int{"b": 2, "a": 1, "B": 3}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"nil":null,"empty":[],"zero":{"A":0},"sorted":{"B":3,"a":1,"b":2}}`; string(out) != want {
		t.Errorf("Marshal = %s, want %s", out, want)
	}
}

func TestJSONNumbers(t *testing.T) {
	var p probe
	if err := json.Unmarshal([]byte(`{"model":"x","unread":1e400}`), &p); err != nil {
		t.Errorf("an out-of-range number in an unread member: %v", err)
	}
	var f float64
	if err := json.Unmarshal([]byte(`1e400`), &f); err == nil {
		t.Error("1e400 decoded into a float64")
	}

	dec := json.NewDecoder(strings.NewReader(`{"n":12345678901234567890}`))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if n, ok := m["n"].(json.Number); !ok || n.String() != "12345678901234567890" {
		t.Errorf("UseNumber decoded %#v, want the json.Number literal", m["n"])
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(`{"n":1}`), &a); err != nil {
		t.Fatal(err)
	}
	if _, ok := a["n"].(float64); !ok {
		t.Errorf("a number in any decoded to %T, want float64", a["n"])
	}
}
