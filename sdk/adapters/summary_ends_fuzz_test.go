package adapters_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// summaryEndsSeeds are edge cases the recorded bodies lack: keys in other cases, summary names inside strings, null and missing members, several choices or candidates, and terminal events that are not last.
var summaryEndsSeeds = []string{
	`{"content":[{"type":"text","text":"\"usage\":{\"output_tokens\":9}"}],"STOP_REASON":"max_tokens","Usage":{"input_tokens":3,"output_tokens":4,"service_tier":"priority"}}`,
	`{"content":[{"type":"tool_use","name":"__relay_structured_output","input":{}}],"stop_reason":"tool_use","usage":{"output_tokens":2}}`,
	`{"id":"x","usage":null,"stop_reason":null}`,
	`{"choices":[{"message":{"content":"a","refusal":"no"},"finish_reason":"stop"},{"message":{"content":"b"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1},"service_tier":"flex"}`,
	`{"choices":[null],"usage":{"prompt_tokens":1}}`,
	`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]},{"type":"function_call","name":"f","arguments":"{}","call_id":"c"}],"usage":{"input_tokens":2,"output_tokens":1}}`,
	`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":2,"output_tokens":1},"service_tier":"default"}`,
	`{"candidates":[{"content":{"parts":[{"text":"a"},{"functionCall":{"name":"f"}}]},"finishReason":"STOP"},{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`,
	`{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}`,
	"event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: response.failed\ndata: {\"response\":null}\n\n",
	"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":4,\"cache_creation_input_tokens\":7}}}\n\nevent: content_block_delta\ndata: {\"delta\":{\"text\":\"event: message_delta\"}}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {}\n\n",
}

// FuzzSummaryEnds checks that whenever an ends reader settles a body, it settles on the Summary the full decode returns. The JSON readers do not validate what they skip, so a sync body must decode in full and hold no duplicate keys; a Responses stream's last terminal event must as well.
func FuzzSummaryEnds(f *testing.F) {
	for _, name := range []string{responseInput, streamInput} {
		paths, err := filepath.Glob(filepath.Join("testdata", "*", "*", name))
		if err != nil {
			f.Fatal(err)
		}
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(b)
		}
	}
	for _, s := range summaryEndsSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, s := range shapes {
			got, settled := endsSummary(s.tr, body)
			if !settled || !endsComparable(s, body) {
				continue
			}
			want, err := v1.ExtractSummary(s.tr, body)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: ends %+v, full decode %+v (%v)", s.name, got, want, err)
			}
		}
	})
}

// endsSummary asks tr's ends reader for body's framing directly.
func endsSummary(tr v1.Translator, body []byte) (v1.Summary, bool) {
	if isStream(body) {
		if r, ok := tr.(v1.StreamEndsSummarizer); ok {
			return r.SummaryFromStreamEnds(body)
		}
		return v1.Summary{}, false
	}
	if r, ok := tr.(v1.JSONEndsSummarizer); ok {
		return r.SummaryFromJSONEnds(body)
	}
	return v1.Summary{}, false
}

func isStream(body []byte) bool {
	head := bytes.TrimLeft(body[:min(len(body), 256)], " \t\r\n\xef\xbb\xbf")
	return bytes.HasPrefix(head, []byte("event:")) || bytes.HasPrefix(head, []byte("data:"))
}

// endsComparable reports whether body is one the ends readers promise to match on.
func endsComparable(s shape, body []byte) bool {
	if !isStream(body) {
		return decodesWhole(s.tr, body)
	}
	if s.name != "openai-responses" {
		return true
	}
	frames, ok := sse.EventFrames(body, "response.completed", "response.incomplete", "response.failed")
	if !ok || len(frames) == 0 {
		return true
	}
	sc := sse.NewScanner(frames[len(frames)-1])
	if !sc.Next() {
		return true
	}
	var ev struct {
		Response json.RawMessage `json:"response"`
	}
	return json.Unmarshal(sc.Data(), &ev) == nil && !hasDuplicateKeys(sc.Data()) && (len(ev.Response) == 0 || decodesWhole(s.tr, ev.Response))
}

func decodesWhole(tr v1.Translator, body []byte) bool {
	_, err := tr.ParseResponse(body)
	return err == nil && !hasDuplicateKeys(body)
}

// hasDuplicateKeys reports whether any object in the JSON b repeats a key, compared the way encoding/json matches field names; unreadable JSON counts as having them.
func hasDuplicateKeys(b []byte) bool {
	dup, err := duplicateKeysIn(json.NewDecoder(bytes.NewReader(b)))
	return dup || err != nil
}

func duplicateKeysIn(dec *json.Decoder) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	switch tok {
	case json.Delim('{'):
		var keys []string
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return false, err
			}
			k, _ := kt.(string)
			if slices.ContainsFunc(keys, func(seen string) bool { return strings.EqualFold(seen, k) }) {
				return true, nil
			}
			keys = append(keys, k)
			if dup, err := duplicateKeysIn(dec); dup || err != nil {
				return dup, err
			}
		}
	case json.Delim('['):
		for dec.More() {
			if dup, err := duplicateKeysIn(dec); dup || err != nil {
				return dup, err
			}
		}
	default:
		return false, nil
	}
	_, err = dec.Token()
	return false, err
}
