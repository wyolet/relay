package adapters_test

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkTranslatorRoundTrip measures each shape's same-shape round trip on
// its recorded tool-calls case: the request parsed to canonical and serialized
// back, and the response the same way. Run it with and without
// GOEXPERIMENT=jsonv2 to compare the JSON engines.
func BenchmarkTranslatorRoundTrip(b *testing.B) {
	cases := []struct{ shape, dir string }{
		{"openai-chat", "testdata/openai/chat-tool-calls"},
		{"openai-responses", "testdata/openai/responses-tool-calls"},
		{"anthropic", "testdata/anthropic/tool-calls"},
		{"gemini", "testdata/gemini/tool-calls"},
	}
	for _, c := range cases {
		s := shapeByName(c.shape)
		reqBody, err := os.ReadFile(filepath.Join(c.dir, requestInput))
		if err != nil {
			b.Fatal(err)
		}
		respBody, err := os.ReadFile(filepath.Join(c.dir, responseInput))
		if err != nil {
			b.Fatal(err)
		}
		req, err := s.tr.ParseRequest(reqBody)
		if err != nil {
			b.Fatal(err)
		}

		b.Run(c.shape+"/request", func(b *testing.B) {
			b.SetBytes(int64(len(reqBody)))
			b.ReportAllocs()
			for b.Loop() {
				r, err := s.tr.ParseRequest(reqBody)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.tr.SerializeRequest(r); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(c.shape+"/response", func(b *testing.B) {
			b.SetBytes(int64(len(respBody)))
			b.ReportAllocs()
			for b.Loop() {
				r, err := s.tr.ParseResponse(respBody)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := s.tr.SerializeResponse(r, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
