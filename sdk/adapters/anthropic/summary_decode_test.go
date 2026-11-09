package anthropic

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var summaryDecodeSeeds = []string{
	`{"content":[{"type":"tool_use","name":"__relay_structured_output","type":null}],"stop_reason":"max_tokens","stop_reason":null}`,
	`{"content":[{"name":"__relay_structured_output","type":"tool_use"},null],"usage":{"output_tokens":2,"service_tier":"priority"}}`,
	`{"content":[{"type":"tool_use","name":"__relay_structured_output"}],"content":[{"name":"x"}]}`,
	`{"content":[5],"usage":{"input_tokens":1}}`,
	`{"content":[{"type":"text","text":5}],"usage":{"input_tokens":1e3}}`,
	`{"usage":{"input_tokens":5},"usage":null,"stop_reason":"refusal"}`,
	"{\"stop_reason\":\"end\\u005fturn\",\"usage\":{\"service_tier\":\"\xff\"}}",
	`[]`, `null`, `{}`, `{"content":[`,
}

func recordedResponses(t testing.TB) [][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "anthropic", "*", "response.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no recorded responses: %v", err)
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
	for _, body := range recordedResponses(t) {
		got, ok := anthropicScanSummary(body)
		want, err := anthropicUnmarshalSummary(body)
		if !ok || err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("scan %+v (settled %v), reference %+v (%v)", got, ok, want, err)
		}
	}
}

// FuzzDecodeSummaryMatchesReference checks DecodeSummary against the encoding/json decoder on any body: the same Summary, and an error exactly when it fails.
func FuzzDecodeSummaryMatchesReference(f *testing.F) {
	for _, b := range recordedResponses(f) {
		f.Add(b)
	}
	for _, s := range summaryDecodeSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, gerr := AnthropicTranslator{}.DecodeSummary(body)
		want, werr := anthropicUnmarshalSummary(body)
		if (gerr == nil) != (werr == nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: DecodeSummary %+v (%v), reference %+v (%v)", body, got, gerr, want, werr)
		}
	})
}
