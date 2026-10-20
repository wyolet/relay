package openai

import (
	"strconv"
	"testing"
)

const benchCCChunkPrefix = `data: {"id":"chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT","object":"chat.completion.chunk","created":1741569952,"model":"gpt-4o-2024-08-06","system_fingerprint":"fp_fc9f1d7035","choices":[`

func benchCCDelta(text string) []byte {
	return []byte(benchCCChunkPrefix + `{"index":0,"delta":{"content":` + strconv.Quote(text) + `},"logprobs":null,"finish_reason":null}]}` + "\n\n")
}

// benchCCStream is a 150-token Chat Completions stream, one token per chunk.
func benchCCStream() [][]byte {
	frames := [][]byte{[]byte(benchCCChunkPrefix + `{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":null}]}` + "\n\n")}
	for i := range 150 {
		frames = append(frames, benchCCDelta(" tok"+strconv.Itoa(i)))
	}
	return append(frames,
		[]byte(benchCCChunkPrefix+`{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`+"\n\n"),
		[]byte(benchCCChunkPrefix+`],"usage":{"prompt_tokens":19,"completion_tokens":150,"total_tokens":169}}`+"\n\n"),
		[]byte("data: [DONE]\n\n"),
	)
}

func BenchmarkCCToCanonicalDelta(b *testing.B) {
	frame := benchCCDelta("Hello")
	s := &ccToCanonicalStream{}
	for _, f := range [][]byte{benchCCStream()[0], frame} {
		if _, err := s.translate(f); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.translate(frame); err != nil {
			b.Fatal(err)
		}
		// Keep the accumulated text from growing across iterations.
		s.msgItem.textBuf = ""
	}
}

func BenchmarkCCToCanonicalStream(b *testing.B) {
	frames := benchCCStream()
	b.ReportAllocs()
	for b.Loop() {
		tc := CCTranslator{}.NewToCanonicalStream()
		for _, f := range frames {
			if _, err := tc(f); err != nil {
				b.Fatal(err)
			}
		}
	}
}
