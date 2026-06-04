// Command mock is a configurable upstream for the relay load-test harness.
// One binary, three modes via MOCK_MODE (or -mode):
//
//   - fast   : reply instantly. Isolates relay's per-request overhead.
//   - slow   : sleep MOCK_LATENCY before replying. Forces concurrency
//     (saturation): N req/s * latency = in-flight count.
//   - stream : reply as OpenAI SSE, one token chunk every MOCK_CHUNK_DELAY,
//     MOCK_CHUNKS chunks. Exercises relay's tee + per-chunk path.
//
// Serves the OpenAI chat-completions shape on /v1/chat/completions and
// /chat/completions so it works behind relay's `openai` adapter. It is NOT a
// fixture replayer — for recorded real-traffic sessions, point relay at
// spec-mock-openai instead (the harness wires it as an optional upstream).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

const completion = `{"id":"chatcmpl-lt","object":"chat.completion","created":1700000000,"model":"loadtest","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}}`

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", env("MOCK_ADDR", ":9990"), "listen address")
	mode := flag.String("mode", env("MOCK_MODE", "fast"), "fast|slow|stream")
	flag.Parse()

	latency, _ := time.ParseDuration(env("MOCK_LATENCY", "2s"))
	chunkDelay, _ := time.ParseDuration(env("MOCK_CHUNK_DELAY", "20ms"))
	chunks, _ := strconv.Atoi(env("MOCK_CHUNKS", "64"))

	h := func(w http.ResponseWriter, r *http.Request) {
		switch *mode {
		case "slow":
			time.Sleep(latency)
			writeJSON(w)
		case "stream":
			writeSSE(w, chunks, chunkDelay)
		default: // fast
			writeJSON(w)
		}
	}
	http.HandleFunc("/v1/chat/completions", h)
	http.HandleFunc("/chat/completions", h)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	fmt.Fprintf(os.Stderr, "mock mode=%s addr=%s latency=%s chunks=%d\n", *mode, *addr, latency, chunks)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	fmt.Fprint(w, completion)
}

func writeSSE(w http.ResponseWriter, chunks int, delay time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	for i := 0; i < chunks; i++ {
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-lt\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t\"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(delay)
	}
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-lt\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
