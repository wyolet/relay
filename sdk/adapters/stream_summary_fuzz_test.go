package adapters_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// summaryFilterSeeds are the edge cases the recorded streams lack: escaped
// and duplicate keys, summary keys spelled inside strings, frames cut
// mid-JSON, and data-only frames where a shape expects event lines.
var summaryFilterSeeds = []string{
	"data: {\"\\u0075sage\":{\"prompt_tokens\":3,\"completion_tokens\":4},\"choices\":[]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"content\":\"\\\"usage\\\":{\\\"prompt_tokens\\\":9}\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",
	"data: {\"service_tier\":\"flex\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"service_tier\":\"fl\\u0065x\",\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: {\"service_tier\":\"default\",\"choices\":[{\"delta\":{\"refusal\":\"no\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2}}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"refusal\":\"no\"},\"delta\":{}}],\"usage\":null,\"usage\":{\"prompt_tokens\":5}}\n\ndata: {\"choices\":[{\"finish_reason\":\"length\",\"finish_reason\":null}]}\n\ndata: [DONE]\n\n",
	"data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"st\n\ndata: {\"usage\":{\"prompt_tokens\":1}\n\ndata: [DONE]\n\n",
	"data: {\"error\":{\"message\":\"x\"}}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n",
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":4,\"service_tier\":\"priority\"}}}\n\nevent: content_block_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":9}}\n\ndata: {\"type\":\"message_stop\"}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	"event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":4}}}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tok\n\nevent: message_stop\ndata: {}\n\n",
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7}}}\n\nevent: response.completed\ndata: {\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n",
	"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\\\"finishReason\\\":\\\"STOP\\\"\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3}}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\"}}]},\"content\":{\"parts\":[]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}\n\n",
	"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}],\"candidates\":[{\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":3}}\n\ndata: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\ndata: {\"candidates\":[{\"conte\n\n",
}

// FuzzSummaryFilter checks that skipping the frames a shape's v1.SummaryStream
// rejects never changes the Summary, buffered or frame by frame, on any body.
func FuzzSummaryFilter(f *testing.F) {
	streams, err := filepath.Glob(filepath.Join("testdata", "*", "*", streamInput))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range streams {
		body, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	for _, s := range summaryFilterSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, s := range shapes {
			all := unfilteredTranslator{s.tr}
			filtered, ferr := v1.ExtractSummary(s.tr, body)
			unfiltered, uerr := v1.ExtractSummary(all, body)
			if (ferr == nil) != (uerr == nil) || !reflect.DeepEqual(filtered, unfiltered) {
				t.Errorf("%s ExtractSummary: filtered %+v (%v), unfiltered %+v (%v)", s.name, filtered, ferr, unfiltered, uerr)
			}
			if got, want := summarizeFrames(s.tr, body), summarizeFrames(all, body); !reflect.DeepEqual(got, want) {
				t.Errorf("%s StreamSummarizer: filtered %+v, unfiltered %+v", s.name, got, want)
			}
		}
	})
}
