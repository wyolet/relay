package adapters_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// BenchmarkStreamSummarizer compares a summarizer that skips the frames a
// shape's v1.SummaryStream rejects with one that translates every frame, over
// each shape's recorded text stream.
func BenchmarkStreamSummarizer(b *testing.B) {
	cases := []struct{ shape, dir string }{
		{"openai-chat", "testdata/openai/chat-text"},
		{"openai-responses", "testdata/openai/responses-text"},
		{"anthropic", "testdata/anthropic/text"},
		{"gemini", "testdata/gemini/text"},
	}
	for _, c := range cases {
		body, err := os.ReadFile(filepath.Join(c.dir, streamInput))
		if err != nil {
			b.Fatal(err)
		}
		var frames [][]byte
		for _, f := range splitFrames(body) {
			frames = append(frames, bytes.TrimRight(f, "\n"))
		}
		tr := shapeByName(c.shape).tr
		for _, v := range []struct {
			name string
			tr   v1.Translator
		}{{"filtered", tr}, {"unfiltered", unfilteredTranslator{tr}}} {
			b.Run(c.shape+"/"+v.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					s := v1.NewStreamSummarizer(v.tr)
					for _, f := range frames {
						s.Observe(f)
					}
					_ = s.Summary()
				}
			})
		}
	}
}
