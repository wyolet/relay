package adapters_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// BenchmarkResponseUsage compares, per buffered response, the two full decodes post-flight ran before (ExtractTokens for the rate-limit commit, ExtractSummary for usage) with the one ends-first read that now serves both, on each shape's recorded text case and on the same case grown to about 200 KB.
func BenchmarkResponseUsage(b *testing.B) {
	cases := []struct{ shape, dir string }{
		{"openai-chat", "testdata/openai/chat-text"},
		{"openai-responses", "testdata/openai/responses-text"},
		{"anthropic", "testdata/anthropic/text"},
		{"gemini", "testdata/gemini/text"},
	}
	const big = 200 << 10
	for _, c := range cases {
		s := shapeByName(c.shape)
		resp, err := os.ReadFile(filepath.Join(c.dir, responseInput))
		if err != nil {
			b.Fatal(err)
		}
		stream, err := os.ReadFile(filepath.Join(c.dir, streamInput))
		if err != nil {
			b.Fatal(err)
		}
		bigStream := inflateStream(s, stream, big/2)
		if c.shape == "openai-responses" {
			// The terminal event repeats the whole response, so a long output is in the stream twice.
			bigStream = inflateLastText(bigStream, big/2)
		}
		bodies := []struct {
			name string
			body []byte
		}{
			{"json-small", resp},
			{"json-200k", inflateJSON(resp, big)},
			{"sse-small", stream},
			{"sse-200k", bigStream},
		}
		for _, in := range bodies {
			b.Run(c.shape+"/"+in.name+"/before", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(in.body)))
				for b.Loop() {
					_ = extractTokens[c.shape](in.body)
					_, _ = v1.ExtractSummary(s.tr, in.body)
				}
			})
			b.Run(c.shape+"/"+in.name+"/after", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(in.body)))
				for b.Loop() {
					_, _, _ = v1.ExtractSummaryEndsFirst(s.tr, in.body)
				}
			})
		}
	}
}

// inflateLastText grows the last text-like string value of body by about size.
func inflateLastText(body []byte, size int) []byte {
	locs := textValue.FindAllIndex(body, -1)
	if len(locs) == 0 {
		return body
	}
	at := locs[len(locs)-1][1]
	return slices.Concat(body[:at], []byte(strings.Repeat(paddingText, size/len(paddingText)+1)), body[at:])
}
