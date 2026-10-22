package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scanResult struct {
	model   string
	code    string // "" when accepted
	msg     string
	closeAt int64 // absolute offset of the top-level close, -1 when not reached
}

// scanChunks feeds b to a fresh scanner in the chunks given by cuts (ascending offsets).
func scanChunks(b []byte, cuts []int) scanResult {
	var s modelScanner
	res := scanResult{closeAt: -1}
	start := 0
	for _, end := range append(cuts, len(b)) {
		if end < start || end > len(b) {
			continue
		}
		if at := s.feed(b[start:end]); at >= 0 {
			res.closeAt = int64(start + at)
		}
		start = end
	}
	if s.err != nil {
		// feed reports no close for the chunk an error is in, wherever the close was.
		res.closeAt = -1
	}
	if err := s.finish(); err != nil {
		res.code, res.msg = err.code, err.msg
	}
	res.model = s.model
	return res
}

func TestModelScanner(t *testing.T) {
	cases := []struct {
		name, body, model, code string
	}{
		{"model only", `{"model":"m"}`, "m", ""},
		{"whitespace everywhere", " \r\n\t{ \"model\" :\t\"m\" ,\n\"x\" : [ 1 , 2 ] }\n ", "m", ""},
		{"model first", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, "m", ""},
		{"model middle", `{"messages":[],"model":"m","max_tokens":5}`, "m", ""},
		{"model last", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":5,"model":"m"}`, "m", ""},
		{"nested model is not top-level", `{"messages":[{"model":"nested"}],"model":"m"}`, "m", ""},
		{"duplicate at depth 2 is fine", `{"model":"m","meta":{"model":"x","model":"y"}}`, "m", ""},
		{"only nested model", `{"messages":[{"model":"nested"}]}`, "", "missing_model"},
		{"string holding model and braces", `{"content":"{\"model\":\"x\"} } ] [ ,","model":"m"}`, "m", ""},
		{"string ending in escaped backslash", `{"a":"\\\\","model":"m"}`, "m", ""},
		{"escaped quote then backslash", `{"a":"\\\"\\","model":"m"}`, "m", ""},
		{"key holding model", `{"model_x":"a","xmodel":"b","model":"m"}`, "m", ""},
		{"unicode escaped key", `{"model":"m"}`, "m", ""},
		{"unicode escaped duplicate", `{"model":"a","model":"b"}`, "", "duplicate_model"},
		{"case-insensitive key", `{"Model":"m"}`, "m", ""},
		{"case-insensitive duplicate", `{"model":"a","MODEL":"b"}`, "", "duplicate_model"},
		{"unicode escapes in value", `{"model":"gpt-4\/o"}`, "gpt-4/o", ""},
		{"non-ascii value", `{"model":"modèl"}`, "modèl", ""},
		{"duplicate at depth 1", `{"model":"a","x":{"y":[1]},"model":"b"}`, "", "duplicate_model"},
		{"null then string", `{"model":null,"model":"x"}`, "", "duplicate_model"},
		{"model null", `{"model":null}`, "", "missing_model"},
		{"model empty", `{"model":""}`, "", "missing_model"},
		{"no model", `{"messages":[]}`, "", "missing_model"},
		{"empty object", `{}`, "", "missing_model"},
		{"model number", `{"model":42}`, "", "parse_error"},
		{"model bool", `{"model":true}`, "", "parse_error"},
		{"model object", `{"model":{"name":"m"}}`, "", "parse_error"},
		{"model array", `{"model":["a","b"]}`, "", "parse_error"},
		{"scalars", `{"model":"m","n":[0,-0,1.5,-1e10,2E+3,3e-2,true,false,null],"o":{}}`, "m", ""},
		{"truncated array", `{"model":"m","x":[1,2`, "", "parse_error"},
		{"truncated string", `{"model":"m`, "", "parse_error"},
		{"truncated after key", `{"model"`, "", "parse_error"},
		{"empty input", ``, "", "parse_error"},
		{"only whitespace", "  \n", "", "parse_error"},
		{"top-level array", `[{"model":"m"}]`, "", "parse_error"},
		{"top-level string", `"model"`, "", "parse_error"},
		{"top-level null", `null`, "", "parse_error"},
		{"trailing value", `{"model":"m"} x`, "", "parse_error"},
		{"second object", `{"model":"m"}{"model":"n"}`, "", "parse_error"},
		{"trailing comma", `{"model":"m",}`, "", "parse_error"},
		{"missing comma", `{"model":"m" "x":1}`, "", "parse_error"},
		{"missing colon", `{"model" "m"}`, "", "parse_error"},
		{"bad literal", `{"model":"m","x":tru}`, "", "parse_error"},
		{"literal with tail", `{"model":"m","x":truex}`, "", "parse_error"},
		{"leading zero", `{"model":"m","x":01}`, "", "parse_error"},
		{"bare minus", `{"model":"m","x":-}`, "", "parse_error"},
		{"bare dot", `{"model":"m","x":1.}`, "", "parse_error"},
		{"bare exponent", `{"model":"m","x":1e}`, "", "parse_error"},
		{"array trailing comma", `{"model":"m","a":[1,]}`, "", "parse_error"},
		{"mismatched close", `{"model":"m","a":[}`, "", "parse_error"},
		{"object closes array", `{"model":"m","a":{]}`, "", "parse_error"},
		{"non-string key", `{1:"m"}`, "", "parse_error"},
		{"bad escape in top-level key", `{"mo\qdel":"m"}`, "", "parse_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte(tc.body)
			whole := scanChunks(b, nil)
			if whole.code != tc.code || (tc.code == "" && whole.model != tc.model) {
				t.Fatalf("got (%q, %q: %s), want (%q, %q)", whole.model, whole.code, whole.msg, tc.model, tc.code)
			}
			for cut := 0; cut <= len(b); cut++ {
				if got := scanChunks(b, []int{cut}); got != whole {
					t.Fatalf("split at %d: %+v, whole: %+v", cut, got, whole)
				}
			}
			bytewise := make([]int, len(b))
			for i := range bytewise {
				bytewise[i] = i
			}
			if got := scanChunks(b, bytewise); got != whole {
				t.Fatalf("byte by byte: %+v, whole: %+v", got, whole)
			}
		})
	}
}

func TestModelScanner_DeepNesting(t *testing.T) {
	for _, depth := range []int{63, 64, 65, 200} {
		var open, close strings.Builder
		for i := range depth {
			if i%3 == 0 {
				open.WriteString(`[`)
				close.WriteString(`]`)
			} else {
				open.WriteString(`{"k":`)
				close.WriteString(`}`)
			}
		}
		inner := open.String() + "1" + reverse(close.String())
		body := `{"a":` + inner + `,"model":"m"}`
		if !json.Valid([]byte(body)) {
			t.Fatalf("depth %d: test body is not valid JSON", depth)
		}
		if got := scanChunks([]byte(body), nil); got.code != "" || got.model != "m" {
			t.Fatalf("depth %d: %+v", depth, got)
		}
	}
	tooDeep := `{"a":` + strings.Repeat("[", maxScanDepth) + strings.Repeat("]", maxScanDepth) + `}`
	if got := scanChunks([]byte(tooDeep), nil); got.code != "parse_error" {
		t.Fatalf("past the depth limit: %+v", got)
	}
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// referenceScan is what encoding/json makes of b's top-level model: ok is false when b is not a valid JSON object, which leaves nothing to compare.
func referenceScan(b []byte) (model, code string, ok bool) {
	if !json.Valid(b) {
		return "", "", false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", "", false
	}
	keys := 0
	var first json.RawMessage
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", "", false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", "", false
		}
		if strings.EqualFold(tok.(string), "model") {
			if keys++; keys == 1 {
				first = raw
			}
		}
	}
	if keys == 0 {
		return "", "missing_model", true
	}
	var s *string
	if err := json.Unmarshal(first, &s); err != nil {
		return "", "parse_error", true
	}
	if keys > 1 {
		return "", "duplicate_model", true
	}
	if s == nil || *s == "" {
		return "", "missing_model", true
	}
	// The single-key case must also be what extractModelStream reads.
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(b, &probe); err != nil || probe.Model != *s {
		panic(fmt.Sprintf("reference disagrees with itself on %q", b))
	}
	return *s, "", true
}

func FuzzModelScanner(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m"}`,
		`{"messages":[{"role":"user","content":"a \"model\": {x}"}],"model":"m"}`,
		`{"model":"a","model":"b"}`,
		`{"Model":"a","x":{"model":"b"}}`,
		`{"a":"\\\\\"","model":"m","n":[1,-2.5e3,true,null]}`,
		`{"model":null}`,
		`{"model":"m"} `,
		`{"model":"m"`,
	} {
		f.Add([]byte(seed), uint64(1))
	}
	f.Fuzz(func(t *testing.T, b []byte, seed uint64) {
		whole := scanChunks(b, nil)

		rng := rand.New(rand.NewPCG(seed, seed>>1|1))
		var cuts []int
		for at := 0; at < len(b); {
			at += 1 + rng.IntN(8)
			cuts = append(cuts, at)
		}
		if got := scanChunks(b, cuts); got != whole {
			t.Fatalf("chunked %v: %+v, whole: %+v", cuts, got, whole)
		}

		model, code, ok := referenceScan(b)
		if !ok {
			return
		}
		if whole.code != code || whole.model != model && code == "" {
			t.Fatalf("scanner (%q, %q: %s), encoding/json (%q, %q) on %q", whole.model, whole.code, whole.msg, model, code, b)
		}
		if code == "" || code == "missing_model" {
			if want := int64(len(bytes.TrimRight(b, " \t\r\n")) - 1); whole.closeAt != want {
				t.Fatalf("close at %d, want %d on %q", whole.closeAt, want, b)
			}
		}
	})
}

// FuzzModelScanner_ValidObjects builds a valid JSON object from each seed, so every input reaches the encoding/json comparison that random bytes rarely do.
func FuzzModelScanner_ValidObjects(f *testing.F) {
	for seed := range uint64(16) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		g := jsonGen{rng: rand.New(rand.NewPCG(seed, ^seed))}
		g.object(0, true)
		b := g.out.Bytes()
		model, code, ok := referenceScan(b)
		if !ok {
			t.Fatalf("generator produced invalid JSON: %q", b)
		}
		whole := scanChunks(b, nil)
		if whole.code != code || whole.model != model && code == "" {
			t.Fatalf("scanner (%q, %q: %s), encoding/json (%q, %q) on %q", whole.model, whole.code, whole.msg, model, code, b)
		}
		var cuts []int
		for at := 0; at < len(b); {
			at += 1 + g.rng.IntN(16)
			cuts = append(cuts, at)
		}
		if got := scanChunks(b, cuts); got != whole {
			t.Fatalf("chunked %v: %+v, whole: %+v on %q", cuts, got, whole, b)
		}
	})
}

func TestModelScanner_AgreesOnGeneratedObjects(t *testing.T) {
	for seed := range uint64(5000) {
		g := jsonGen{rng: rand.New(rand.NewPCG(seed, ^seed))}
		g.object(0, true)
		b := g.out.Bytes()
		model, code, ok := referenceScan(b)
		if !ok {
			t.Fatalf("seed %d: generator produced invalid JSON: %q", seed, b)
		}
		if got := scanChunks(b, nil); got.code != code || got.model != model && code == "" {
			t.Fatalf("seed %d: scanner (%q, %q: %s), encoding/json (%q, %q) on %q", seed, got.model, got.code, got.msg, model, code, b)
		}
	}
}

// jsonGen writes random valid JSON whose top-level keys are biased towards spellings of "model".
type jsonGen struct {
	rng *rand.Rand
	out bytes.Buffer
}

var genKeys = []string{`"model"`, `"Model"`, `"MODEL"`, `"model"`, `"model"`, `"messages"`, `"x"`, `"modelx"`, `"m\"odel"`, `"mod"`, `"\\model"`}

var genStrings = []string{`""`, `"m"`, `"gpt-4o"`, `"a \"model\": {\"x\"}"`, `"\\"`, `"\\\\\""`, `"{[,:]}"`, `"café"`, `"😀"`, `"tab\there\nnewline"`, `"modèl"`, `"\/"`}

func (g *jsonGen) ws() {
	for range g.rng.IntN(3) {
		g.out.WriteByte(" \t\r\n"[g.rng.IntN(4)])
	}
}

func (g *jsonGen) object(depth int, top bool) {
	g.ws()
	g.out.WriteByte('{')
	n := g.rng.IntN(5)
	if top {
		n++
	}
	for i := range n {
		if i > 0 {
			g.out.WriteByte(',')
		}
		g.ws()
		g.out.WriteString(genKeys[g.rng.IntN(len(genKeys))])
		g.ws()
		g.out.WriteByte(':')
		g.value(depth + 1)
	}
	g.ws()
	g.out.WriteByte('}')
	g.ws()
}

func (g *jsonGen) value(depth int) {
	g.ws()
	kind := g.rng.IntN(10)
	if depth > 4 && kind >= 7 {
		kind = 0
	}
	switch kind {
	case 0, 1, 2, 3:
		g.out.WriteString(genStrings[g.rng.IntN(len(genStrings))])
	case 4:
		g.out.WriteString([]string{"0", "-0", "12", "-3.25", "1e9", "2E-3", "0.5e+2"}[g.rng.IntN(7)])
	case 5:
		g.out.WriteString([]string{"true", "false"}[g.rng.IntN(2)])
	case 6:
		g.out.WriteString("null")
	case 7, 8:
		g.out.WriteByte('[')
		for i := range g.rng.IntN(4) {
			if i > 0 {
				g.out.WriteByte(',')
			}
			g.value(depth + 1)
		}
		g.ws()
		g.out.WriteByte(']')
	default:
		g.object(depth, false)
	}
	g.ws()
}

func TestExtractModelStream_DuplicateModel(t *testing.T) {
	for _, body := range []string{
		`{"model":"cheap","messages":[],"model":"expensive"}`,
		`{"model":"cheap","Model":"expensive"}`,
		`{"model":null,"model":"expensive"}`,
	} {
		if _, _, err := extractModelStream([]byte(body)); err != errDuplicateModel {
			t.Errorf("%s: err = %v, want errDuplicateModel", body, err)
		}
	}
	if m, _, err := extractModelStream([]byte(`{"model":"m","meta":{"model":"a","model":"b"}}`)); err != nil || m != "m" {
		t.Errorf("nested duplicate: (%q, %v), want (m, nil)", m, err)
	}
}

func BenchmarkModelScanner(b *testing.B) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"source-files", claudeCodeSizedBody(b, 350<<10)},
		{"quote-every-17-bytes", denseQuoteBody(350 << 10)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := scanModel(tc.body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// claudeCodeSizedBody is a coding-agent-like chat body of about size bytes: this package's Go sources as tool results between short turns, the model last.
func claudeCodeSizedBody(tb testing.TB, size int) []byte {
	tb.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		tb.Fatalf("no source files: %v", err)
	}
	var msgs []any
	for i, n := 0, 0; n < size; i++ {
		src, err := os.ReadFile(files[i%len(files)])
		if err != nil {
			tb.Fatal(err)
		}
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("t%d", i), "name": "Read", "input": map[string]any{"file_path": files[i%len(files)]}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": string(src)}}},
		)
		n += len(src)
	}
	b, err := json.Marshal(map[string]any{"max_tokens": 4096, "stream": true, "system": "You are a coding agent.", "messages": msgs})
	if err != nil {
		tb.Fatal(err)
	}
	return append(b[:len(b)-1], `,"model":"test-model"}`...)
}

// denseQuoteBody is a body of about size bytes whose strings hold an escaped quote about every 17 bytes, the worst case for a scan that stops at each quote.
func denseQuoteBody(size int) []byte {
	chunk := `def handler(req):\n    return {\"status\": 200, \"body\": req.get(\"q\")}\n\t# {braces} [brackets] \\ path\n`
	var msgs strings.Builder
	for i := 0; msgs.Len() < size; i++ {
		if i > 0 {
			msgs.WriteByte(',')
		}
		fmt.Fprintf(&msgs, `{"role":"user","content":[{"type":"text","text":"%s"},{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}`, strings.Repeat(chunk, 8), i, strings.Repeat(chunk, 4))
	}
	return []byte(`{"max_tokens":4096,"stream":true,"system":"be brief","messages":[` + msgs.String() + `],"model":"test-model"}`)
}
