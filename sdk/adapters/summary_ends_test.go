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

// endsInput is one body the ends readers are checked on.
type endsInput struct {
	shape   shape
	name    string
	variant string
	body    []byte
}

// paddingText is JSON string content with the escapes real content carries, and none of the names the ends readers search for.
const paddingText = `Some \"quoted\" words, a tab\t and a newline\n, then more of the same. `

var textValue = regexp.MustCompile(`"(text|content|thinking)"\s*:\s*"`)

// inflateJSON grows the first text-like string value of body past the ends window, or returns nil when it has none.
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

func gzipped(t *testing.T, b []byte) []byte {
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

// recordedInputs returns every recorded response and stream under testdata, with variants past the ends window, gzipped and, for streams, with CRLF line ends.
func recordedInputs(t *testing.T) []endsInput {
	t.Helper()
	var out []endsInput
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
		name := filepath.ToSlash(path)
		add := func(variant string, b []byte) {
			if b != nil {
				out = append(out, endsInput{shape: s, name: name, variant: variant, body: b})
			}
		}
		add("recorded", body)
		add("gzip", gzipped(t, body))
		if filepath.Base(path) == responseInput {
			add("past window", inflateJSON(body, 3<<16))
			continue
		}
		add("past window", inflateStream(s, body, 3<<16))
		add("crlf", bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")))
	}
	return out
}

func globInputs(t *testing.T, name string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*", name))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no recorded %s: %v", name, err)
	}
	return paths
}

// endsReader reports whether the shape has an ends reader for body's framing.
func endsReader(tr v1.Translator, body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		_, ok := tr.(v1.StreamEndsSummarizer)
		return ok
	}
	_, ok := tr.(v1.JSONEndsSummarizer)
	return ok
}

// wantFallback names the inputs whose Summary the ends readers leave to the full decode, by shape and variant.
var wantFallback = map[string]string{
	"openai-chat past window": "choices[0] is only told from the last choice by walking the whole body",
	"gemini past window":      "candidates[0] is only told from the last candidate by walking the whole body",
	"anthropic crlf":          "named events are only searched for in LF streams",
	"openai-responses crlf":   "named events are only searched for in LF streams",
}

// TestSummaryFromEndsMatchesFullDecode checks, on every recorded response and stream and their variants, that the ends-first Summary is the one the full decode returns, and that the ends readers settle it wherever they are meant to.
func TestSummaryFromEndsMatchesFullDecode(t *testing.T) {
	for _, in := range recordedInputs(t) {
		t.Run(in.name+"/"+in.variant, func(t *testing.T) {
			want, werr := v1.ExtractSummary(in.shape.tr, in.body)
			got, fellBack, gerr := v1.ExtractSummaryEndsFirst(in.shape.tr, in.body)
			if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("ends first %+v (%v), full decode %+v (%v)", got, gerr, want, werr)
			}
			hasReader := endsReader(in.shape.tr, plainBody(in.body))
			why, listed := wantFallback[in.shape.name+" "+in.variant]
			switch {
			case hasReader && listed && !fellBack:
				t.Errorf("settled at the ends, but wantFallback lists it: %s", why)
			case hasReader && !listed && fellBack:
				t.Errorf("the ends reader fell back to the full decode")
			}
		})
	}
}

// TestResponsesEndsMembersBetweenWindows covers a Responses body whose members sit between the head and tail windows: a member the walks miss must never read as absent, and output items out of reach must still be able to change the finish reason.
func TestResponsesEndsMembersBetweenWindows(t *testing.T) {
	pad := strings.Repeat(paddingText, (3<<16)/len(paddingText))
	message := `{"type":"message","content":[{"type":"output_text","text":"` + pad + `"}]}`
	tools := `"tools":[{"type":"function","name":"f","description":"` + pad + `"}]`
	cases := map[string]string{
		"usage between":         `{"status":"completed","output":[` + message + `],"usage":{"input_tokens":4,"output_tokens":2},` + tools + `,"service_tier":"default"}`,
		"usage in another case": `{"status":"completed","output":[` + message + `],"USAGE":{"input_tokens":4,"output_tokens":2},` + tools + `,"service_tier":"default"}`,
		"call out of reach":     `{"status":"completed","output":[{"type":"function_call","name":"f","arguments":"{}","call_id":"c"},` + message + `],` + tools + `,"usage":{"input_tokens":4},"service_tier":"default"}`,
		"refusal out of reach":  `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]},` + message + `],"usage":{"input_tokens":4},"service_tier":"default"}`,
		"all within the ends":   `{"status":"completed","output":[` + message + `],"usage":{"input_tokens":4,"output_tokens":2},"service_tier":"default"}`,
	}
	tr := shapeByName("openai-responses").tr
	for name, body := range cases {
		want, _ := v1.ExtractSummary(tr, []byte(body))
		got, fellBack, _ := v1.ExtractSummaryEndsFirst(tr, []byte(body))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ends first %+v, full decode %+v", name, got, want)
		}
		if settles := name == "all within the ends"; settles == fellBack {
			t.Errorf("%s: fell back = %v", name, fellBack)
		}
	}
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

// TestSummaryFromEndsSpecMockCorpus runs the same check over a spec-mock fixture corpus (JSONL replay fixtures, recorded upstream responses), when RELAY_SPEC_MOCK_FIXTURES names a directory holding one.
func TestSummaryFromEndsSpecMockCorpus(t *testing.T) {
	root := os.Getenv("RELAY_SPEC_MOCK_FIXTURES")
	if root == "" {
		t.Skip("RELAY_SPEC_MOCK_FIXTURES not set")
	}
	type tally struct{ bodies, settled, fellBack, tokensDiffer int }
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
			s := shapeByName(name)
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
			want, _ := v1.ExtractSummary(s.tr, body)
			got, fellBack, _ := v1.ExtractSummaryEndsFirst(s.tr, body)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s:%d: ends first %+v, full decode %+v", path, line, got, want)
			}
			if fellBack {
				tl.fellBack++
			} else if endsReader(s.tr, plainBody(body)) {
				tl.settled++
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
		tl := tallies[key]
		t.Logf("%s: %d bodies, %d settled at the ends, %d fell back, %d where ExtractTokens differs", key, tl.bodies, tl.settled, tl.fellBack, tl.tokensDiffer)
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
