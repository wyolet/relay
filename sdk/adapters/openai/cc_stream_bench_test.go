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
		s.msgItem.textBuf.Reset()
	}
}

func BenchmarkCCToCanonicalStream(b *testing.B) {
	benchCCToCanonical(b, benchCCStream())
}

// benchLongDeltas is how many ~8-token deltas make a ~4,000-token stream.
const benchLongDeltas = 500

// benchLongDeltaText is the i-th ~8-token delta of a long stream.
func benchLongDeltaText(i int) string {
	return " word" + strconv.Itoa(i) + " alpha beta gamma delta epsilon zeta eta"
}

// benchCCLongStream is a ~4,000-token Chat Completions stream whose deltas
// each carry the JSON-encoded body built by delta.
func benchCCLongStream(first string, delta func(i int) string, finish string) [][]byte {
	frames := [][]byte{[]byte(benchCCChunkPrefix + `{"index":0,"delta":` + first + `,"logprobs":null,"finish_reason":null}]}` + "\n\n")}
	for i := range benchLongDeltas {
		frames = append(frames, []byte(benchCCChunkPrefix+`{"index":0,"delta":`+delta(i)+`,"logprobs":null,"finish_reason":null}]}`+"\n\n"))
	}
	return append(frames,
		[]byte(benchCCChunkPrefix+`{"index":0,"delta":{},"logprobs":null,"finish_reason":"`+finish+`"}]}`+"\n\n"),
		[]byte(benchCCChunkPrefix+`],"usage":{"prompt_tokens":19,"completion_tokens":4000,"total_tokens":4019}}`+"\n\n"),
		[]byte("data: [DONE]\n\n"),
	)
}

func BenchmarkCCToCanonicalLongStream(b *testing.B) {
	b.Run("text", func(b *testing.B) {
		benchCCToCanonical(b, benchCCLongStream(`{"role":"assistant","content":""}`, func(i int) string {
			return `{"content":` + strconv.Quote(benchLongDeltaText(i)) + `}`
		}, "stop"))
	})
	b.Run("reasoning", func(b *testing.B) {
		benchCCToCanonical(b, benchCCLongStream(`{"role":"assistant","content":""}`, func(i int) string {
			return `{"reasoning_content":` + strconv.Quote(benchLongDeltaText(i)) + `}`
		}, "stop"))
	})
	b.Run("tool_args", func(b *testing.B) {
		first := `{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":"{\"text\":\""}}]}`
		benchCCToCanonical(b, benchCCLongStream(first, func(i int) string {
			return `{"tool_calls":[{"index":0,"function":{"arguments":` + strconv.Quote(benchLongDeltaText(i)) + `}}]}`
		}, "tool_calls"))
	})
}

func benchCCToCanonical(b *testing.B, frames [][]byte) {
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
