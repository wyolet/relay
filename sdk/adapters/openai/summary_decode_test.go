package openai

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// summaryDecoders pairs each shape's one-pass decoder with the encoding/json decoder it must match.
var summaryDecoders = []struct {
	name      string
	scan      func([]byte) (v1.Summary, bool)
	decode    func([]byte) (v1.Summary, error)
	reference func([]byte) (v1.Summary, error)
	dir       string
}{
	{"chat", ccScanSummary, CCTranslator{}.DecodeSummary, ccUnmarshalSummary, "chat-*"},
	{"responses", responsesScanSummary, ResponsesTranslator{}.DecodeSummary, referenceResponsesSummary, "responses-*"},
}

var summaryDecodeSeeds = []string{
	`{"choices":[{"message":{"refusal":"no","refusal":null},"finish_reason":"stop","finish_reason":null}],"service_tier":"a","service_tier":null}`,
	`{"choices":[{"finish_reason":"length"}],"choices":[{"message":{"refusal":"no"}}]}`,
	`{"choices":[null,{"message":5}],"usage":{"prompt_tokens":1.5}}`,
	`{"choices":[{"message":{"refusal":"x"}},{"message":{},"message":null}]}`,
	`{"status":"completed","output":[{"content":[{"type":"refusal"}],"type":"message"},{"type":"function_call","content":5}]}`,
	`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","type":null},null]}],"output":null}`,
	`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":1}}}`,
	"{\"service_tier\":\"bad \xff\",\"status\":\"x\\ty\"}",
	`[]`, `null`, `{}`, `{"choices":`,
}

// recordedResponses returns the recorded response bodies of the cases matching pattern.
func recordedResponses(t testing.TB, pattern string) [][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "openai", pattern, "response.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no recorded %s responses: %v", pattern, err)
	}
	var out [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// TestScanSummarySettlesRecordedResponses checks the one-pass read decides every recorded response itself, without falling back to encoding/json.
func TestScanSummarySettlesRecordedResponses(t *testing.T) {
	for _, d := range summaryDecoders {
		for _, body := range recordedResponses(t, d.dir) {
			got, ok := d.scan(body)
			want, err := d.reference(body)
			if !ok || err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: scan %+v (settled %v), reference %+v (%v)", d.name, got, ok, want, err)
			}
		}
	}
}

// FuzzDecodeSummaryMatchesReference checks DecodeSummary against the encoding/json decoder on any body: the same Summary, and an error exactly when it fails.
func FuzzDecodeSummaryMatchesReference(f *testing.F) {
	for _, d := range summaryDecoders {
		for _, b := range recordedResponses(f, d.dir) {
			f.Add(b)
		}
	}
	for _, s := range summaryDecodeSeeds {
		f.Add([]byte(s))
	}
	f.Add([]byte(strings.Repeat(`{"choices":[`, 600)))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, d := range summaryDecoders {
			got, gerr := d.decode(body)
			want, werr := d.reference(body)
			if (gerr == nil) != (werr == nil) || !reflect.DeepEqual(got, want) {
				t.Errorf("%s %q: DecodeSummary %+v (%v), reference %+v (%v)", d.name, body, got, gerr, want, werr)
			}
		}
	})
}
