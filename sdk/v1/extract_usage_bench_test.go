package v1

import (
	"bytes"
	"testing"
)

// benchStreamFrames is a canonical text stream of 104 frames, each without
// its blank-line separator, as a stream session hands them to Observe.
func benchStreamFrames() [][]byte {
	frames := [][]byte{
		[]byte(`event: generation.created` + "\n" + `data: {"id":"gen_1","model":"m"}`),
		[]byte(`event: item.started` + "\n" + `data: {"item_id":"i1","index":0,"item":{"type":"message","role":"assistant"}}`),
	}
	for range 100 {
		frames = append(frames, []byte(`event: item.delta`+"\n"+`data: {"item_id":"i1","index":0,"kind":"text","delta":"token "}`))
	}
	return append(frames,
		[]byte(`event: item.completed`+"\n"+`data: {"item_id":"i1","index":0}`),
		[]byte(`event: generation.completed`+"\n"+`data: {"id":"gen_1","status":"completed","usage":{"input":120,"output":100},"finish_reason":"stop"}`),
	)
}

var benchTranslators = []struct {
	name string
	tr   Translator
}{
	{"canonical", fakeTranslator{}},
	{"translated", passTranslator{}},
}

func BenchmarkStreamSummarizerObserve(b *testing.B) {
	frames := benchStreamFrames()
	for _, bt := range benchTranslators {
		b.Run(bt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s := NewStreamSummarizer(bt.tr)
				for _, f := range frames {
					s.Observe(f)
				}
				if s.Summary().Tokens["output"] != 100 {
					b.Fatal("usage not harvested")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(frames)), "ns/frame")
		})
	}
}

func BenchmarkExtractSummarySSE(b *testing.B) {
	body := append(bytes.Join(benchStreamFrames(), []byte("\n\n")), '\n', '\n')
	for _, bt := range benchTranslators {
		b.Run(bt.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s, err := ExtractSummary(bt.tr, body)
				if err != nil || s.Tokens["output"] != 100 {
					b.Fatal("usage not harvested")
				}
			}
		})
	}
}
