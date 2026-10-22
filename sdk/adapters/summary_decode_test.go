package adapters_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// extractTokens is each shape's ExtractTokens, the extractor the rate-limit commit read on buffered bodies before it shared the response Summary.
var extractTokens = map[string]func([]byte) usage.Tokens{
	"openai-chat":      openai.ExtractTokens,
	"openai-responses": openai.ExtractTokens,
	"anthropic":        anthropic.ExtractTokens,
	"gemini":           gemini.ExtractTokens,
}

// extractTokensDiffers names the shapes whose ExtractTokens reads usage differently from their translator, and why.
var extractTokensDiffers = map[string]string{
	"openai-responses": "the shape shares the Chat Completions ExtractTokens, which reads prompt_tokens/completion_tokens; Responses usage is input_tokens/output_tokens, so it finds none",
}

// fullSummary is ExtractSummary without the shape's narrow decoder or stream predicate: ParseResponse, or every frame translated.
func fullSummary(tr v1.Translator, body []byte) (v1.Summary, error) {
	return v1.ExtractSummary(unfilteredTranslator{tr}, body)
}

// decodeInput is one body the narrow decoders are checked on.
type decodeInput struct {
	shape   shape
	name    string
	variant string
	body    []byte
}

// paddingText is JSON string content with the escapes real content carries.
const paddingText = `Some \"quoted\" words, a tab\t and a newline\n, then more of the same. `

var textValue = regexp.MustCompile(`"(text|content|thinking)"\s*:\s*"`)

// inflateJSON grows the first text-like string value of body to about size, or returns nil when it has none.
func inflateJSON(body []byte, size int) []byte {
	loc := textValue.FindIndex(body)
	if loc == nil {
		return nil
	}
	pad := strings.Repeat(paddingText, size/len(paddingText)+1)
	return slices.Concat(body[:loc[1]], []byte(pad), body[loc[1]:])
}

// inflateStream repeats the first frame the shape's summary predicate skips until body passes size, or returns nil when every frame matters.
func inflateStream(s shape, body []byte, size int) []byte {
	ss, ok := s.tr.(v1.SummaryStream)
	if !ok {
		return nil
	}
	_, affects := ss.NewSummaryStream()
	frames := splitFrames(body)
	for i, f := range frames {
		if affects(bytes.TrimRight(f, "\n")) {
			continue
		}
		out := slices.Concat(frames[:i+1]...)
		for len(out) < size {
			out = append(out, f...)
		}
		return append(out, slices.Concat(frames[i+1:]...)...)
	}
	return nil
}

func gzipped(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// recordedInputs returns every recorded response and stream under testdata, as recorded, gzipped and grown to about 200 KB, and streams with CRLF line ends.
func recordedInputs(t testing.TB) []decodeInput {
	t.Helper()
	var out []decodeInput
	for _, path := range slices.Concat(globInputs(t, responseInput), globInputs(t, streamInput)) {
		dir := filepath.Dir(path)
		s, ok := shapeForCase(filepath.Base(filepath.Dir(dir)), filepath.Base(dir))
		if !ok {
			t.Fatalf("%s: no shape owns this case", dir)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		add := func(variant string, b []byte) {
			if b != nil {
				out = append(out, decodeInput{shape: s, name: filepath.ToSlash(path), variant: variant, body: b})
			}
		}
		add("recorded", body)
		add("gzip", gzipped(t, body))
		if filepath.Base(path) == responseInput {
			add("200k", inflateJSON(body, 200<<10))
			continue
		}
		add("200k", inflateStream(s, body, 200<<10))
		add("crlf", bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")))
	}
	return out
}

func globInputs(t testing.TB, name string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*", name))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no recorded %s: %v", name, err)
	}
	return paths
}

// TestSummaryDecodeMatchesFullDecode checks every recorded response and stream, and their variants: ExtractSummary, with the narrow decoder, returns the Summary of the full decode.
func TestSummaryDecodeMatchesFullDecode(t *testing.T) {
	for _, in := range recordedInputs(t) {
		want, werr := fullSummary(in.shape.tr, in.body)
		got, gerr := v1.ExtractSummary(in.shape.tr, in.body)
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("%s %s: narrow %+v (%v), full %+v (%v)", in.name, in.variant, got, gerr, want, werr)
		}
	}
}

// TestSummaryDecodeTruncated checks every prefix of every recorded body: a cut sync body fails both decodes, and a cut stream summarizes alike.
func TestSummaryDecodeTruncated(t *testing.T) {
	for _, in := range recordedInputs(t) {
		if in.variant != "recorded" {
			continue
		}
		d, isJSON := in.shape.tr.(v1.SummaryDecoder)
		for n := range len(in.body) {
			prefix := in.body[:n]
			want, _ := fullSummary(in.shape.tr, prefix)
			got, _ := v1.ExtractSummary(in.shape.tr, prefix)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s cut at %d: narrow %+v, full %+v", in.name, n, got, want)
			}
			if isJSON && filepath.Base(in.name) == responseInput && n < len(bytes.TrimRight(in.body, " \t\r\n")) {
				if _, err := d.DecodeSummary(prefix); err == nil {
					t.Fatalf("%s cut at %d: the narrow decoder accepted a truncated body", in.name, n)
				}
			}
		}
	}
}

// summaryDecodeCases are sync bodies the recorded ones lack: duplicate, case-folded and escaped keys, several choices or candidates, nulls, and members that decide the finish reason.
var summaryDecodeCases = map[string][]string{
	"anthropic": {
		`{"usage":{"input_tokens":5},"usage":{"output_tokens":3},"stop_reason":"max_tokens"}`,
		`{"Usage":{"INPUT_TOKENS":5,"service_tier":"priority"},"STOP_REASON":"tool_use"}`,
		`{"usage":{"input_tokens":5},"stop_reason":"refusal"}`,
		`{"content":[{"type":"tool_use","name":"__relay_structured_output","input":{}}],"stop_reason":"max_tokens","usage":{"output_tokens":2}}`,
		`{"content":[{"type":"tool_use","name":"__relay_structured_output"}],"content":[{"type":"text"}],"stop_reason":"tool_use"}`,
		`{"content":[{"type":"tool_use","name":"__relay_structured_output"}],"content":[{"name":"x"}],"stop_reason":"tool_use"}`,
		`{"content":null,"usage":null,"stop_reason":null}`,
		`{"content":"text","usage":{"input_tokens":1}}`,
		`{"content":[{"type":"text","text":5}],"usage":{"input_tokens":1}}`,
		`null`,
		`[]`,
	},
	"openai-chat": {
		`{"choices":[{"message":{"refusal":"no"},"finish_reason":"stop"},{"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1},"service_tier":"flex"}`,
		`{"choices":[{"finish_reason":"length"}],"choices":[{"message":{"refusal":"no"}}],"usage":{"prompt_tokens":5},"usage":{"completion_tokens":2}}`,
		`{"CHOICES":[{"Message":{"Refusal":"no"},"Finish_Reason":"stop"}],"Usage":{"prompt_tokens":1},"Service_Tier":"x"}`,
		`{"choices":[null],"usage":null}`,
		`{"choices":[{"message":null,"finish_reason":"content_filter"}]}`,
		`{"choices":[{"message":{"content":[{"type":"text","text":"a"}],"refusal":null},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":"x"}]}`,
		`{"choices":{}}`,
		`{"choices":[{"finish_reason":"length"}]}`,
		`null`,
	},
	"openai-responses": {
		`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]},{"type":"function_call","name":"f","arguments":"{}","call_id":"c"}],"usage":{"input_tokens":2,"output_tokens":1}}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}],"service_tier":"default"}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"output_audio"}]}],"usage":{"input_tokens":2}}`,
		`{"status":"completed","output":[{"type":"function_call","content":5}],"usage":{"input_tokens":2}}`,
		`{"status":"completed","output":[{"type":"message","content":5}]}`,
		`{"status":"completed","output":[{"type":"message","content":"plain"},null,{"type":"web_search_call"}]}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"a"}],"content":[{"type":"refusal"}]}]}`,
		`{"status":"completed","output":[{"type":"function_call"}],"output":[{"type":"message","content":null}]}`,
		`{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"incomplete_details":{},"usage":{"input_tokens":1}}`,
		`{"STATUS":"incomplete","Incomplete_Details":{"Reason":"max_output_tokens"},"USAGE":{"input_tokens":1},"Service_Tier":"flex"}`,
		`{"status":"failed","output":[{"type":"custom_tool_call"}]}`,
		`{"status":"completed","output":[{"type":"message","content":[null]}]}`,
		`{"status":"completed","output":[{"type":"function_call"}],"output":[{}]}`,
		`{"status":"completed","output":[{"type":"function_call"},{"type":"message","content":[{"type":"refusal"}]}]}`,
		`{"status":"completed","output":[{"type":"message","type":null,"content":[{"type":"refusal","type":"output_text"}]}]}`,
		`{"status":"completed","output":[{"type":5}]}`,
		`{"status":"completed","output":[7]}`,
		`{"status":"completed","output":[{"type":"message","content":{}}]}`,
		`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text"},5]}]}`,
		`null`,
	},
	"gemini": {
		`{"candidates":[{"content":{"parts":[{"text":"a"},{"functionCall":{"name":"f"}}]},"finishReason":"STOP"},{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"},"functionCall":null}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}],"candidates":[{"content":{"parts":[{"text":"b"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":"f"}]}}]}`,
		`{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}`,
		`{"promptFeedback":null,"UsageMetadata":{"PromptTokenCount":4}}`,
		`{"candidates":[null],"usageMetadata":null}`,
		`{"candidates":[{"content":null,"finishReason":"MAX_TOKENS"}]}`,
		`null`,
	},
}

// TestSummaryDecodeEdgeCases checks the narrow decoders on summaryDecodeCases against the full decode: the same Summary, and an error exactly when ParseResponse fails on a member the Summary reads.
func TestSummaryDecodeEdgeCases(t *testing.T) {
	for name, bodies := range summaryDecodeCases {
		s := shapeByName(name)
		for _, body := range bodies {
			want, _ := fullSummary(s.tr, []byte(body))
			got, _ := v1.ExtractSummary(s.tr, []byte(body))
			if !reflect.DeepEqual(got, want) && !mistypesUnreadMember(s, body) {
				t.Errorf("%s %s: narrow %+v, full %+v", name, body, got, want)
			}
		}
	}
}

// mistypesUnreadMember names the cases ParseResponse rejects only for a member the Summary does not read; the narrow decoder still returns their Summary.
func mistypesUnreadMember(s shape, body string) bool {
	return slices.Contains([]string{
		`{"content":[{"type":"text","text":5}],"usage":{"input_tokens":1}}`,
	}, body) && s.name == "anthropic"
}

// TestSummaryTokensMatchExtractTokens pins where the response Summary's tokens, which the rate-limit commit now reads, differ from ExtractTokens on the recorded bodies.
func TestSummaryTokensMatchExtractTokens(t *testing.T) {
	differed := map[string]bool{}
	for _, in := range recordedInputs(t) {
		if in.variant != "recorded" {
			continue
		}
		sum, err := v1.ExtractSummary(in.shape.tr, in.body)
		if err != nil {
			t.Fatal(err)
		}
		old := extractTokens[in.shape.name](in.body)
		same := maps.Equal(old, sum.Tokens)
		if _, known := extractTokensDiffers[in.shape.name]; !known && !same {
			t.Errorf("%s: ExtractTokens %v, Summary %v", in.name, old, sum.Tokens)
		}
		differed[in.shape.name] = differed[in.shape.name] || !same
	}
	for name, why := range extractTokensDiffers {
		if !differed[name] {
			t.Errorf("%s no longer differs from ExtractTokens on any recorded body; drop its extractTokensDiffers entry (%s)", name, why)
		}
	}
}

// TestUnfinishedStreamsKeepTheirTokens shows why the rate-limit commit asks ExtractTokens when the Summary has no tokens: a stream that ends on an error event, or before its terminal frame, completes no generation, yet reported the usage the upstream served.
func TestUnfinishedStreamsKeepTheirTokens(t *testing.T) {
	cases := []struct{ shape, body string }{
		{"anthropic", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":30}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"},
		{"openai-chat", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"},
	}
	for _, c := range cases {
		sum, err := v1.ExtractSummary(shapeByName(c.shape).tr, []byte(c.body))
		if err != nil || len(sum.Tokens) != 0 {
			t.Errorf("%s: Summary tokens %v (%v), want none: the case no longer needs the ExtractTokens fallback", c.shape, sum.Tokens, err)
		}
		if got := extractTokens[c.shape]([]byte(c.body)); len(got) == 0 {
			t.Errorf("%s: ExtractTokens found no tokens", c.shape)
		}
	}
}

// specMockFixture is the part of a spec-mock replay fixture these tests read.
type specMockFixture struct {
	Match struct {
		Path string `json:"path"`
	} `json:"match"`
	Response struct {
		Body      json.RawMessage `json:"body"`
		SSEChunks []struct {
			Data string `json:"data"`
		} `json:"sse_chunks"`
	} `json:"response"`
}

var specMockShapes = map[string]string{
	"/v1/chat/completions": "openai-chat",
	"/v1/responses":        "openai-responses",
	"/v1/messages":         "anthropic",
}

// TestSummaryDecodeSpecMockCorpus runs the same check over a spec-mock fixture corpus (JSONL replay fixtures of recorded upstream responses), when RELAY_SPEC_MOCK_FIXTURES names a directory holding one.
func TestSummaryDecodeSpecMockCorpus(t *testing.T) {
	root := os.Getenv("RELAY_SPEC_MOCK_FIXTURES")
	if root == "" {
		t.Skip("RELAY_SPEC_MOCK_FIXTURES not set")
	}
	type tally struct{ bodies, tokensDiffer int }
	tallies := map[string]*tally{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 64<<20)
		for line := 1; sc.Scan(); line++ {
			var fx specMockFixture
			if json.Unmarshal(sc.Bytes(), &fx) != nil {
				continue
			}
			name, ok := specMockShapes[fx.Match.Path]
			if !ok {
				continue
			}
			body, framing := specMockBody(fx)
			key := name + " " + framing
			if tallies[key] == nil {
				tallies[key] = &tally{}
			}
			tl := tallies[key]
			tl.bodies++
			if body == nil {
				continue
			}
			tr := shapeByName(name).tr
			want, _ := fullSummary(tr, body)
			got, _ := v1.ExtractSummary(tr, body)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s:%d: narrow %+v, full %+v", path, line, got, want)
			}
			if !maps.Equal(extractTokens[name](plainBody(body)), want.Tokens) {
				tl.tokensDiffer++
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range slices.Sorted(maps.Keys(tallies)) {
		t.Logf("%s: %d bodies, %d where ExtractTokens differs", key, tallies[key].bodies, tallies[key].tokensDiffer)
	}
}

// specMockBody rebuilds the wire body a fixture replays: the JSON body (a string holds raw bytes, one per rune, as recorded) or the SSE chunks. A string body holding U+FFFD lost bytes when it was recorded, so it is reported unreadable.
func specMockBody(fx specMockFixture) ([]byte, string) {
	if len(fx.Response.SSEChunks) > 0 {
		var b []byte
		for _, c := range fx.Response.SSEChunks {
			b = append(append(append(b, "data: "...), c.Data...), "\n\n"...)
		}
		return b, "stream"
	}
	raw := fx.Response.Body
	if len(raw) == 0 {
		return nil, ""
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return raw, "json"
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		return nil, "unreadable"
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			return []byte(s), "json"
		}
		b = append(b, byte(r))
	}
	return b, "json"
}

// plainBody is body gunzipped when it is gzip, as the extractors expect it.
func plainBody(body []byte) []byte {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return body
	}
	return out.Bytes()
}
