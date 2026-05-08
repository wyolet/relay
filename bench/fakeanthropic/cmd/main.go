// Command fakeanthropic runs a minimal fake Anthropic Messages API that replays
// captured Claude Code session responses.
//
// Usage:
//
//	go run ./bench/fakeanthropic/cmd --session ~/.claude/projects/<encoded>/<id>.jsonl --listen :9999
//
// Optional latency injection (uniform random per request) for load testing:
//
//	go run ./bench/fakeanthropic/cmd --min-latency 2s --max-latency 20s
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wyolet/relay/bench/fakeanthropic"
)

func main() {
	var (
		session    = flag.String("session", defaultSession(), "path to Claude Code session JSONL")
		listen     = flag.String("listen", ":9999", "listen address")
		minLatency = flag.Duration("min-latency", 0, "minimum simulated upstream latency per request")
		maxLatency = flag.Duration("max-latency", 0, "maximum simulated upstream latency per request (0 disables)")
	)
	flag.Parse()

	responses, err := fakeanthropic.LoadSession(*session)
	if err != nil {
		log.Fatalf("load session: %v", err)
	}
	log.Printf("loaded %d assistant turns from %s", len(responses), *session)
	log.Printf("listening on %s (latency=%s..%s)", *listen, *minLatency, *maxLatency)

	srv := fakeanthropic.New(responses)
	srv.LatencyMin = *minLatency
	srv.LatencyMax = *maxLatency

	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := hs.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func defaultSession() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", "session.jsonl")
}
