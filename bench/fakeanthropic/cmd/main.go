// Command fakeanthropic runs a minimal fake Anthropic Messages API that replays
// captured Claude Code session responses.
//
// Usage:
//
//	go run ./bench/fakeanthropic/cmd --session ~/.claude/projects/<encoded>/<id>.jsonl --listen :9999
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wyolet/relay/bench/fakeanthropic"
)

func main() {
	var (
		session = flag.String("session", defaultSession(), "path to Claude Code session JSONL")
		listen  = flag.String("listen", ":9999", "listen address")
	)
	flag.Parse()

	responses, err := fakeanthropic.LoadSession(*session)
	if err != nil {
		log.Fatalf("load session: %v", err)
	}
	log.Printf("loaded %d assistant turns from %s", len(responses), *session)
	log.Printf("listening on %s", *listen)

	srv := fakeanthropic.New(responses)
	if err := http.ListenAndServe(*listen, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func defaultSession() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", "session.jsonl")
}
