package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var (
	vendorKeys    = openai.CCTranslator{}.RequestKeys()
	canonicalKeys = v1.IdentityTranslator{}.RequestKeys()
)

func TestRequestKeysPerShape(t *testing.T) {
	for name, got := range map[string]v1.RequestKeys{
		"chat":      openai.CCTranslator{}.RequestKeys(),
		"responses": openai.ResponsesTranslator{}.RequestKeys(),
		"messages":  anthropic.AnthropicTranslator{}.RequestKeys(),
	} {
		if got != (v1.RequestKeys{Stream: "stream"}) {
			t.Errorf("%s: %+v, want the stream bool", name, got)
		}
	}
	if canonicalKeys != (v1.RequestKeys{Stream: "output_mode", StreamOn: "stream"}) {
		t.Errorf("canonical: %+v", canonicalKeys)
	}
}

// requestCode is the API error code the route answers body with, "" when accepted.
func requestCode(body []byte, keys v1.RequestKeys) (*ParsedRequest, string) {
	req, rej := parseRequest(body, keys)
	switch {
	case rej != nil:
		return nil, rej.code
	case req.Model == "":
		return req, "missing_model"
	}
	return req, ""
}

func TestParseRequest_ErrorParity(t *testing.T) {
	cases := []struct {
		name, body, code string
	}{
		{"plain", `{"model":"m","stream":true}`, ""},
		{"leading and trailing space", " \n{\"model\":\"m\"}\t\r\n", ""},
		{"empty body", ``, "parse_error"},
		{"whitespace only", " \n", "parse_error"},
		{"top-level null", `null`, "missing_model"},
		{"top-level null with space", " null ", "missing_model"},
		{"top-level array", `[{"model":"m"}]`, "parse_error"},
		{"top-level string", `"m"`, "parse_error"},
		{"top-level number", `1`, "parse_error"},
		{"invalid top-level", `nul`, "parse_error"},
		{"missing model", `{"messages":[]}`, "missing_model"},
		{"empty object", `{}`, "missing_model"},
		{"model null", `{"model":null}`, "missing_model"},
		{"model empty", `{"model":""}`, "missing_model"},
		{"model number", `{"model":1}`, "parse_error"},
		{"model object", `{"model":{"a":1}}`, "parse_error"},
		{"model array", `{"model":["a"]}`, "parse_error"},
		{"model bool", `{"model":false}`, "parse_error"},
		{"model escaped", `{"model":"a` + uEscape("00e9") + `\"b"}`, ""},
		{"model invalid utf-8", "{\"model\":\"a\xffb\"}", ""},
		{"stream string", `{"model":"m","stream":"true"}`, "parse_error"},
		{"stream number", `{"model":"m","stream":1}`, "parse_error"},
		{"stream null", `{"model":"m","stream":null}`, ""},
		{"Stream other case string", `{"model":"m","STREAM":"x"}`, "parse_error"},
		{"duplicate model", `{"model":"a","model":"b"}`, "duplicate_model"},
		{"duplicate model other case", `{"model":"a","Model":"b"}`, "duplicate_model"},
		{"duplicate model escaped", `{"model":"a","` + uEscape("006d") + `odel":"b"}`, "duplicate_model"},
		{"duplicate null model", `{"model":null,"model":"b"}`, "duplicate_model"},
		{"duplicate after a type error", `{"model":"a","stream":"x","model":"b"}`, "parse_error"},
		{"duplicate before a syntax error", `{"model":"a","model":"b",}`, "parse_error"},
		{"model wrong type and duplicated", `{"model":1,"model":"b"}`, "parse_error"},
		{"nested duplicate model", `{"model":"m","x":{"model":"a","model":"b"}}`, ""},
		{"other case model", `{"Model":"m"}`, ""},
		{"escaped model key", `{"` + uEscape("006d") + `odel":"m"}`, ""},
		{"trailing comma", `{"model":"m",}`, "parse_error"},
		{"trailing data", `{"model":"m"}x`, "parse_error"},
		{"second object", `{"model":"m"}{}`, "parse_error"},
		{"truncated", `{"model":"m","messages":[`, "parse_error"},
		{"control character in a string", "{\"model\":\"m\",\"x\":\"a\tb\"}", "parse_error"},
		{"control character in a key", "{\"model\":\"m\",\"a\nb\":1}", "parse_error"},
		{"invalid escape", `{"model":"m","x":"\q"}`, "parse_error"},
		{"invalid escape in a nested key", `{"model":"m","x":{"\q":1}}`, "parse_error"},
		{"short unicode escape", `{"model":"m","x":"` + uEscape("12") + `"}`, "parse_error"},
		{"bad hex in unicode escape", `{"model":"m","x":"` + uEscape("12g4") + `"}`, "parse_error"},
		{"lone surrogate", `{"model":"m","x":"` + uEscape("d800") + `"}`, ""},
		{"invalid utf-8 elsewhere", "{\"model\":\"m\",\"x\":\"\xc3\x28\"}", ""},
		{"byte order mark", "\xef\xbb\xbf{\"model\":\"m\"}", "parse_error"},
		{"leading zero", `{"model":"m","x":01}`, "parse_error"},
		{"bare minus", `{"model":"m","x":-}`, "parse_error"},
		{"plus sign", `{"model":"m","x":+1}`, "parse_error"},
		{"dot without digits", `{"model":"m","x":1.}`, "parse_error"},
		{"exponent without digits", `{"model":"m","x":1e+}`, "parse_error"},
		{"numbers", `{"model":"m","x":[0,-0,1.5,-1e10,2E+3,3e-2,1e400,123456789012345678901234567890]}`, ""},
		{"NaN", `{"model":"m","x":NaN}`, "parse_error"},
		{"single quotes", `{"model":'m'}`, "parse_error"},
		{"comment", `{"model":"m"/*c*/}`, "parse_error"},
		{"literal with tail", `{"model":"m","x":truex}`, "parse_error"},
		{"capital literal", `{"model":"m","x":True}`, "parse_error"},
		{"mismatched close", `{"model":"m","x":[1}`, "parse_error"},
		{"missing colon", `{"model" "m"}`, "parse_error"},
		{"non-string key", `{"model":"m",1:2}`, "parse_error"},
		{"array trailing comma", `{"model":"m","x":[1,]}`, "parse_error"},
		{"empty containers", `{"model":"m","a":{},"b":[],"c":[{}],"d":{"e":[]}}`, ""},
		{"html characters", `{"model":"m","x":"<a href=\"x\">&amp;</a>","y":"` + uEscape("2028") + `"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte(tc.body)
			if _, code := requestCode(b, vendorKeys); code != tc.code {
				t.Fatalf("code %q, want %q", code, tc.code)
			}
			if want := probeCode(b); want != tc.code {
				t.Fatalf("the probe decode answered %q, the table says %q", want, tc.code)
			}
		})
	}
}

// The probe decode read both stream spellings on every route; each shape now reads only its own.
func TestParseRequest_StreamPerShape(t *testing.T) {
	cases := []struct {
		name, body        string
		vendor, canonical bool
	}{
		{"stream true", `{"model":"m","stream":true}`, true, false},
		{"stream false", `{"model":"m","stream":false}`, false, false},
		{"output_mode stream", `{"model":"m","output_mode":"stream"}`, false, true},
		{"output_mode sync", `{"model":"m","output_mode":"sync"}`, false, false},
		{"neither", `{"model":"m"}`, false, false},
		{"stream null", `{"model":"m","stream":null}`, false, false},
		{"escaped output_mode value", `{"model":"m","output_mode":"str` + uEscape("0065") + `am"}`, false, true},
		{"escaped exact stream key", `{"model":"m","str` + uEscape("0065") + `am":true}`, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []struct {
				keys v1.RequestKeys
				want bool
			}{{vendorKeys, tc.vendor}, {canonicalKeys, tc.canonical}} {
				req, rej := parseRequest([]byte(tc.body), k.keys)
				if rej != nil {
					t.Fatalf("%+v: %v", k.keys, rej)
				}
				if req.Stream != k.want {
					t.Errorf("%+v: stream = %v, want %v", k.keys, req.Stream, k.want)
				}
			}
		})
	}
	if _, rej := parseRequest([]byte(`{"model":"m","output_mode":1}`), canonicalKeys); rej == nil || rej.code != "parse_error" {
		t.Errorf("canonical output_mode number: %v, want parse_error", rej)
	}
}

// longS is U+017F, which Unicode case folding (and so encoding/json's key matching) equates with 's'.
const longS = "ſ"

// A stream key relay and a case-sensitive or first-wins upstream could read differently is refused, so relay never takes a stream for a buffered call.
func TestParseRequest_StreamKeySpelling(t *testing.T) {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(`{"`+longS+`tream":true}`), &probe); err != nil || !probe.Stream {
		t.Fatalf("encoding/json no longer folds U+017F onto 's' (%v), so this case tests nothing", err)
	}
	cases := []struct {
		name, body string
		keys       v1.RequestKeys
		code       string
	}{
		{"other case after exact", `{"model":"m","stream":true,"Stream":false}`, vendorKeys, "duplicate_stream"},
		{"exact duplicate with null", `{"model":"m","stream":true,"stream":null}`, vendorKeys, "duplicate_stream"},
		{"exact duplicate", `{"model":"m","stream":false,"stream":true}`, vendorKeys, "duplicate_stream"},
		{"escaped duplicate", `{"model":"m","stream":true,"str` + uEscape("0065") + `am":false}`, vendorKeys, "duplicate_stream"},
		{"fold duplicate", `{"model":"m","stream":true,"` + longS + `tream":false}`, vendorKeys, "duplicate_stream"},
		{"other case alone", `{"model":"m","Stream":true}`, vendorKeys, "invalid_stream_key"},
		{"upper case alone", `{"model":"m","STREAM":false}`, vendorKeys, "invalid_stream_key"},
		{"fold alone", `{"model":"m","` + longS + `tream":true}`, vendorKeys, "invalid_stream_key"},
		{"escaped other case", `{"model":"m","` + uEscape("0053") + `tream":true}`, vendorKeys, "invalid_stream_key"},
		{"canonical duplicate", `{"model":"m","output_mode":"stream","output_mode":null}`, canonicalKeys, "duplicate_stream"},
		{"canonical other case", `{"model":"m","Output_Mode":"stream"}`, canonicalKeys, "invalid_stream_key"},
		{"type error first", `{"model":"m","stream":"x","Stream":true}`, vendorKeys, "parse_error"},
		{"duplicate model first", `{"model":"m","model":"n","stream":true,"stream":true}`, vendorKeys, "duplicate_model"},
		{"before missing model", `{"stream":true,"Stream":true}`, vendorKeys, "duplicate_stream"},
		{"shape without a stream key", `{"model":"m","stream":true,"Stream":false}`, v1.RequestKeys{}, ""},
		{"vendor key on the canonical route", `{"model":"m","stream":true,"Stream":false}`, canonicalKeys, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, code := requestCode([]byte(tc.body), tc.keys); code != tc.code {
				t.Fatalf("code %q, want %q", code, tc.code)
			}
			if _, _, code := referenceRequest([]byte(tc.body), tc.keys); code != tc.code {
				t.Fatalf("reference code %q, want %q", code, tc.code)
			}
		})
	}
}

func TestHandleShape_StreamKeyRefused(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	spec := &adapter.Spec{Name: adapters.OpenAI, Translator: openai.CCTranslator{}}
	for body, code := range map[string]string{
		`{"model":"test-model","stream":true,"Stream":false,"messages":[]}`: "duplicate_stream",
		`{"model":"test-model","stream":true,"stream":null,"messages":[]}`:  "duplicate_stream",
		`{"model":"test-model","` + longS + `tream":true,"messages":[]}`:    "invalid_stream_key",
	} {
		r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body)), prin)
		w := httptest.NewRecorder()
		handleShape(spec, d, w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", body, w.Code, w.Body)
		}
		if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Type != "invalid_request_error" || e.Error.Code != code {
			t.Fatalf("%s: envelope %s/%s, want invalid_request_error/%s", body, e.Error.Type, e.Error.Code, code)
		}
	}
}

func TestParseRequest_DeepNesting(t *testing.T) {
	ok := `{"model":"m","a":` + strings.Repeat("[", maxScanDepth-1) + strings.Repeat("]", maxScanDepth-1) + `}`
	if _, code := requestCode([]byte(ok), vendorKeys); code != "" || !json.Valid([]byte(ok)) {
		t.Fatalf("at the depth limit: %q", code)
	}
	tooDeep := `{"model":"m","a":` + strings.Repeat("[", maxScanDepth) + strings.Repeat("]", maxScanDepth) + `}`
	if _, code := requestCode([]byte(tooDeep), vendorKeys); code != "parse_error" || json.Valid([]byte(tooDeep)) {
		t.Fatalf("past the depth limit: %q", code)
	}
}

func TestParsedRequestMember(t *testing.T) {
	req, rej := parseRequest([]byte(`{"model":"m","x":1,"X":2,"x":[3],"`+uEscape("0079")+`":"y"}`), vendorKeys)
	if rej != nil {
		t.Fatal(rej)
	}
	for key, want := range map[string]string{"x": "[3]", "X": "2", "y": `"y"`, "z": ""} {
		if got := string(req.Member(key)); got != want {
			t.Errorf("Member(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestUpstreamBody(t *testing.T) {
	cases := []struct {
		name, body, model, want string
	}{
		{"same model forwards the original", `{"model":"m", "x":1}`, "m", `{"model":"m", "x":1}`},
		{"new model spliced in place", "{ \"x\" : 1 ,\n \"model\" : \"m\" , \"y\":[2] }\n", "up", "{ \"x\" : 1 ,\n \"model\" : \"up\" , \"y\":[2] }\n"},
		{"escaped key keeps its spelling", `{"` + uEscape("006d") + `odel":"m","x":1}`, "up", `{"` + uEscape("006d") + `odel":"up","x":1}`},
		{"other case key gets a model member", `{"Model":"m"}`, "m", `{"Model":"m","model":"m"}`},
		{"no model member", `{"x":1}`, "up", `{"x":1,"model":"up"}`},
		{"empty object", `{}`, "up", `{"model":"up"}`},
		{"name needing escapes", `{"model":"m"}`, "a\"b\\c\x01<é>", `{"model":"a\"b\\c` + uEscape("0001") + `<é>"}`},
		{"duplicates keep the last", `{"x":1,"model":"m","x":2}`, "up", `{"model":"up","x":2}`},
		{"duplicates untouched when nothing changes", `{"x":1,"model":"m","x":2}`, "m", `{"x":1,"model":"m","x":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, rej := parseRequest([]byte(tc.body), vendorKeys)
			if rej != nil {
				t.Fatal(rej)
			}
			got := req.upstreamBody(tc.model, nil)
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			// The map path quoted the name with strconv.Quote and gave up on one JSON cannot hold.
			if json.Valid([]byte(strconv.Quote(tc.model))) {
				ref, _ := rewriteViaMap([]byte(tc.body), tc.model, nil)
				assertSameJSON(t, got, ref)
			}
		})
	}
}

func TestStripParams(t *testing.T) {
	gemini := map[string]string{"temperature": "g.temperature", "top_p": "g.topP", "top_k": "g.k.top"}
	cases := []struct {
		name, body  string
		unsupported []string
		paths       map[string]string
		want        string
		dropped     []string
	}{
		{"top-level", `{"model":"m","temperature":0.7,"top_p":0.9,"messages":[]}`, []string{"temperature", "top_p"}, ccPaths, `{"model":"up","messages":[]}`, []string{"temperature", "top_p"}},
		{"first member", `{ "temperature":0.7, "model":"m"}`, []string{"temperature"}, ccPaths, `{"model":"up"}`, []string{"temperature"}},
		{"last member", `{"model":"m","temperature":0.7 }`, []string{"temperature"}, ccPaths, `{"model":"up" }`, []string{"temperature"}},
		{"absent", `{"model":"m"}`, []string{"temperature"}, ccPaths, `{"model":"up"}`, nil},
		{"null counts as present", `{"model":"m","temperature":null}`, []string{"temperature"}, ccPaths, `{"model":"up"}`, []string{"temperature"}},
		{"unmapped param", `{"model":"m","top_k":5}`, []string{"top_k"}, ccPaths, `{"model":"up","top_k":5}`, nil},
		{"param listed twice", `{"model":"m","temperature":1}`, []string{"temperature", "temperature"}, ccPaths, `{"model":"up"}`, []string{"temperature"}},
		{"duplicate keys all go", `{"temperature":1,"model":"m","temperature":2}`, []string{"temperature"}, ccPaths, `{"model":"up"}`, []string{"temperature"}},
		{"nested", `{"model":"m","g":{"temperature":0.7,"max":5}}`, []string{"temperature"}, gemini, `{"model":"up","g":{"max":5}}`, []string{"temperature"}},
		{"nested twice in one parent", `{"model":"m","g":{"temperature":0.7,"topP":1,"max":5}}`, []string{"temperature", "top_p"}, gemini, `{"model":"up","g":{"max":5}}`, []string{"temperature", "top_p"}},
		{"nested two levels", `{"model":"m","g":{"k":{"top":3,"x":1}}}`, []string{"top_k"}, gemini, `{"model":"up","g":{"k":{"x":1}}}`, []string{"top_k"}},
		{"nested parent not an object", `{"model":"m","g":[1]}`, []string{"temperature"}, gemini, `{"model":"up","g":[1]}`, nil},
		{"nested parent null", `{"model":"m","g":null}`, []string{"temperature"}, gemini, `{"model":"up","g":null}`, nil},
		{"nested absent", `{"model":"m","g":{"max":5}}`, []string{"temperature"}, gemini, `{"model":"up","g":{"max":5}}`, nil},
		{"no paths", `{"model":"m","temperature":1}`, []string{"temperature"}, nil, `{"model":"up","temperature":1}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, rej := parseRequest([]byte(tc.body), vendorKeys)
			if rej != nil {
				t.Fatal(rej)
			}
			edits, dropped := req.stripParams(nil, tc.unsupported, tc.paths)
			got := req.upstreamBody("up", edits)
			if string(got) != tc.want || !slices.Equal(dropped, tc.dropped) {
				t.Fatalf("got %s %v, want %s %v", got, dropped, tc.want, tc.dropped)
			}
			ref, _, refDropped := bytePassViaMap([]byte(tc.body), "up", false, tc.unsupported, tc.paths)
			assertSameJSON(t, got, ref)
			if !slices.Equal(dropped, refDropped) {
				t.Fatalf("dropped %v, the map strip dropped %v", dropped, refDropped)
			}
		})
	}
}

func TestStripParamsLeavesUnchangedBodyAlone(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	req, rej := parseRequest(body, vendorKeys)
	if rej != nil {
		t.Fatal(rej)
	}
	edits, dropped := req.stripParams(nil, []string{"temperature"}, ccPaths)
	if out := req.upstreamBody("m", edits); dropped != nil || &out[0] != &body[0] {
		t.Fatalf("a body with nothing to change was copied: dropped=%v", dropped)
	}
}

// onePassBody is what byte-pass sends upstream on the new path, for the comparisons with bytePassViaMap.
func onePassBody(body []byte, upstreamModel string, usageOptIn bool, unsupported []string, paths map[string]string) (out []byte, usage bool, dropped []string, rej *bodyError) {
	req, rej := parseRequest(body, vendorKeys)
	if rej != nil {
		return nil, false, nil, rej
	}
	var edits []memberEdit
	if usageOptIn {
		if key, value, ok := openai.RequestStreamUsage(req.Member); ok {
			edits = append(edits, memberEdit{key: key, value: value})
			usage = true
		}
	}
	if len(unsupported) > 0 {
		edits, dropped = req.stripParams(edits, unsupported, paths)
	}
	return req.upstreamBody(upstreamModel, edits), usage, dropped, nil
}

// checkAgainstMap asserts the one-pass body is the map round trip's up to member order and whitespace, and that every member it did not edit is forwarded byte for byte, in order.
func checkAgainstMap(t *testing.T, body []byte, upstreamModel string, usageOptIn bool, unsupported []string) {
	t.Helper()
	got, usage, dropped, rej := onePassBody(body, upstreamModel, usageOptIn, unsupported, ccPaths)
	if rej != nil {
		t.Fatalf("parse: %v", rej)
	}
	want, wantUsage, wantDropped := bytePassViaMap(body, upstreamModel, usageOptIn, unsupported, ccPaths)
	if usage != wantUsage || !slices.Equal(dropped, wantDropped) {
		t.Fatalf("usage %v dropped %v, the map path: usage %v dropped %v", usage, dropped, wantUsage, wantDropped)
	}
	if bytes.Equal(want, body) != bytes.Equal(got, body) {
		t.Fatalf("one path forwarded the body unchanged and the other did not")
	}
	assertSameJSON(t, got, want)
	edited := append([]string{"model", "stream_options"}, dropped...)
	if a, b := untouchedMembers(t, body, edited), untouchedMembers(t, got, edited); !slices.Equal(a, b) {
		t.Fatalf("untouched members differ:\n in  %q\n out %q", a, b)
	}
}

// untouchedMembers lists an object's members as sent, skipping the edited keys and members a later duplicate overrides.
func untouchedMembers(t *testing.T, b []byte, edited []string) []string {
	t.Helper()
	obj, err := scanJSONObject(b, nil)
	if err != nil || !obj.isObject {
		t.Fatalf("not an object: %v", err)
	}
	skip := obj.overridden()
	var out []string
	for i, m := range obj.members {
		if skip != nil && skip[i] || slices.Contains(edited, obj.keyString(i)) {
			continue
		}
		out = append(out, string(b[m.keyStart-1:m.valEnd]))
	}
	return out
}

func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	if !json.Valid(got) {
		t.Fatalf("output is not valid JSON: %q", got)
	}
	if a, b := decodeAny(t, got), decodeAny(t, want); !reflect.DeepEqual(a, b) {
		t.Fatalf("not the same JSON:\n got  %s\n want %s", got, want)
	}
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return v
}

func TestUpstreamBodyMatchesMapOnFixtures(t *testing.T) {
	files, err := filepath.Glob("../../../sdk/adapters/testdata/*/*/request*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no request fixtures: %v", err)
	}
	bodies := map[string][]byte{
		"claude-code-sized":      claudeCodeSizedBody(t, 400<<10, false),
		"claude-code-model-last": claudeCodeSizedBody(t, 64<<10, true),
		"dense-quotes":           denseQuoteBody(64 << 10),
		"small":                  smallChatBody(),
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		bodies[f] = b
	}
	for name, body := range bodies {
		t.Run(filepath.Base(filepath.Dir(name))+"/"+filepath.Base(name), func(t *testing.T) {
			if _, rej := parseRequest(body, vendorKeys); rej != nil {
				t.Fatalf("parse: %v", rej)
			}
			// Make the body a stream so the usage opt-in has something to do, without touching its members.
			streamed := body
			if req, _ := parseRequest(body, vendorKeys); req.Member("stream") == nil {
				streamed = req.obj.assemble([]memberEdit{{key: "stream", value: []byte("true")}})
			}
			for _, upstream := range []string{"upstream-model", "a\"b\\c<é>"} {
				checkAgainstMap(t, body, upstream, false, nil)
				checkAgainstMap(t, body, upstream, false, []string{"temperature", "top_p"})
				checkAgainstMap(t, streamed, upstream, true, nil)
				checkAgainstMap(t, streamed, upstream, true, []string{"temperature"})
			}
			if req, _ := parseRequest(body, vendorKeys); req.Model != "" {
				checkAgainstMap(t, body, req.Model, false, nil)
			}
		})
	}
}

// smallChatBody is a one-turn Chat Completions request of about 2 KB.
func smallChatBody() []byte {
	return []byte(`{"model":"test-model","max_tokens":1024,"temperature":0.2,"stream":true,"messages":[{"role":"system","content":"` +
		strings.Repeat("You are a careful assistant. ", 30) + `"},{"role":"user","content":"` +
		strings.Repeat(`Summarise the \"plan\" below.\n`, 40) + `"}],"user":"u-123"}`)
}

// FuzzParseRequest compares the one-pass parse with encoding/json on any input: the same bodies refused, the same model and stream read, and an assembled body that is valid JSON meaning what the map round trip meant.
func FuzzParseRequest(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","stream":true,"temperature":0.5,"messages":[{"role":"user","content":"a \"model\" {x}"}]}`,
		`{"model":"m","stream_options":{"include_usage":false,"x":"<&>"},"stream":true}`,
		`{"Model":"m","STREAM":true,"temperature":null}`,
		`{"model":"a","` + uEscape("006d") + `odel":"b"}`,
		`{"model":"m","x":"` + uEscape("d800") + `","y":[1,-2.5e3,true,null,{}],"x":2}`,
		`{"model":"m","stream":"true"}`,
		`{"model":1}`,
		`null`,
		"{\"model\":\"m\",\"x\":\"\t\"}",
		`{"model":"m"`,
	} {
		f.Add([]byte(seed), "up")
	}
	f.Fuzz(func(t *testing.T, b []byte, upstream string) {
		if len(b) > 4<<10 {
			return
		}
		obj, err := scanJSONObject(b, nil)
		if (err == nil) != json.Valid(b) {
			t.Fatalf("scanner err %v, json.Valid %v on %q", err, json.Valid(b), b)
		}
		if err == nil && obj.isObject && !bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte("{")) {
			t.Fatalf("object not starting with a brace: %q", b)
		}
		for _, keys := range []v1.RequestKeys{vendorKeys, canonicalKeys} {
			req, code := requestCode(b, keys)
			wantModel, wantStream, wantCode := referenceRequest(b, keys)
			if code != wantCode || code == "" && (req.Model != wantModel || req.Stream != wantStream) {
				t.Fatalf("%+v: (%q, %v, %q), encoding/json (%q, %v, %q) on %q", keys, modelOf(req), req != nil && req.Stream, code, wantModel, wantStream, wantCode, b)
			}
		}
		if _, code := requestCode(b, vendorKeys); code != "" {
			return
		}
		if !json.Valid([]byte(strconv.Quote(upstream))) {
			// The map path gave up on a name strconv.Quote cannot write as JSON; the one-pass path writes it.
			got, _, _, _ := onePassBody(b, upstream, false, nil, nil)
			var fields map[string]json.RawMessage
			var model string
			if err := json.Unmarshal(got, &fields); err != nil || json.Unmarshal(fields["model"], &model) != nil {
				t.Fatalf("model %q written as %q: %v", upstream, got, err)
			}
			if utf8.ValidString(upstream) && model != upstream {
				t.Fatalf("model %q read back as %q", upstream, model)
			}
			return
		}
		for _, usage := range []bool{false, true} {
			got, gotUsage, gotDropped, _ := onePassBody(b, upstream, usage, []string{"temperature", "top_p"}, ccPaths)
			want, wantUsage, wantDropped := bytePassViaMap(b, upstream, usage, []string{"temperature", "top_p"}, ccPaths)
			if gotUsage != wantUsage || !slices.Equal(gotDropped, wantDropped) {
				t.Fatalf("usage %v dropped %v, map: %v %v on %q", gotUsage, gotDropped, wantUsage, wantDropped, b)
			}
			if bytes.Equal(want, b) {
				if !bytes.Equal(got, b) {
					t.Fatalf("the map path forwarded %q unchanged, the one-pass path sent %q", b, got)
				}
				continue
			}
			if !json.Valid(want) {
				// The map path's strconv.Quote wrote a name JSON cannot hold and sent invalid JSON; the one-pass path must still be valid.
				if !json.Valid(got) {
					t.Fatalf("invalid output %q", got)
				}
				continue
			}
			assertSameJSON(t, got, want)
		}
	})
}

func modelOf(req *ParsedRequest) string {
	if req == nil {
		return ""
	}
	return req.Model
}

// referenceRequest is what encoding/json reads from b for a shape with keys: a probe decode of the members the shape names, then the duplicate-model check.
func referenceRequest(b []byte, keys v1.RequestKeys) (model string, stream bool, code string) {
	var err error
	if keys.StreamOn == "" {
		var probe struct {
			Model  string `json:"model"`
			Stream *bool  `json:"stream"`
		}
		err = json.Unmarshal(b, &probe)
		model, stream = probe.Model, probe.Stream != nil && *probe.Stream
	} else {
		var probe struct {
			Model      string `json:"model"`
			OutputMode string `json:"output_mode"`
		}
		err = json.Unmarshal(b, &probe)
		model, stream = probe.Model, probe.OutputMode == keys.StreamOn
	}
	if err != nil {
		return "", false, "parse_error"
	}
	if _, refCode, ok := referenceScan(b); ok && refCode == errDuplicateModel.code {
		return "", false, refCode
	}
	if keys.Stream != "" {
		var matches []string
		for _, k := range topLevelKeys(b) {
			if strings.EqualFold(k, keys.Stream) {
				matches = append(matches, k)
			}
		}
		switch {
		case len(matches) > 1:
			return "", false, "duplicate_stream"
		case len(matches) == 1 && matches[0] != keys.Stream:
			return "", false, "invalid_stream_key"
		}
	}
	if model == "" {
		return "", false, "missing_model"
	}
	return model, stream, ""
}

// topLevelKeys lists an object's top-level keys as encoding/json decodes them; none for anything else.
func topLevelKeys(b []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}

func TestParseRequest_AgreesOnGeneratedObjects(t *testing.T) {
	for seed := range uint64(2000) {
		g := jsonGen{rng: rand.New(rand.NewPCG(seed, ^seed))}
		g.object(0, true)
		b := g.out.Bytes()
		for _, keys := range []v1.RequestKeys{vendorKeys, canonicalKeys} {
			req, code := requestCode(b, keys)
			wantModel, wantStream, wantCode := referenceRequest(b, keys)
			if code != wantCode || code == "" && (req.Model != wantModel || req.Stream != wantStream) {
				t.Fatalf("seed %d %+v: (%q, %q), encoding/json (%q, %v, %q) on %q", seed, keys, modelOf(req), code, wantModel, wantStream, wantCode, b)
			}
		}
		if _, code := requestCode(b, vendorKeys); code == "" {
			checkAgainstMap(t, b, fmt.Sprintf("up-%d", seed%3), false, []string{"temperature"})
		}
	}
}

func BenchmarkBytePassBody(b *testing.B) {
	for _, size := range []struct {
		name string
		body []byte
	}{
		{"2KB", smallChatBody()},
		{"400KB", claudeCodeSizedBody(b, 400<<10, false)},
	} {
		for _, upstream := range []string{"test-model", "test-model-2026-01-01"} {
			change := "same-model"
			if upstream != "test-model" {
				change = "new-model"
			}
			b.Run(size.name+"/"+change+"/map", func(b *testing.B) {
				b.SetBytes(int64(len(size.body)))
				b.ReportAllocs()
				for b.Loop() {
					if _, _, err := probeModelStream(size.body); err != nil {
						b.Fatal(err)
					}
					_, _, _ = bytePassViaMap(size.body, upstream, false, nil, nil)
				}
			})
			b.Run(size.name+"/"+change+"/one-pass", func(b *testing.B) {
				b.SetBytes(int64(len(size.body)))
				b.ReportAllocs()
				for b.Loop() {
					req, rej := parseRequest(size.body, vendorKeys)
					if rej != nil {
						b.Fatal(rej)
					}
					_ = req.upstreamBody(upstream, nil)
				}
			})
		}
	}
}
