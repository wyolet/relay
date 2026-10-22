package sse_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// splitEventFrames is EventFrames the slow way: split every frame and keep those a translator would read as one of names.
func splitEventFrames(b []byte, names ...string) [][]byte {
	out := [][]byte{}
	for rest := b; len(rest) > 0; {
		n, frame, _ := v1.SplitSSEFrames(rest, true)
		if n == 0 {
			break
		}
		rest = rest[n:]
		if sc := sse.NewScanner(frame); sc.Next() && slices.Contains(names, string(sc.Event())) {
			out = append(out, frame)
		}
	}
	return out
}

var eventNames = []string{"message_start", "message_delta", "message_stop", "response.completed", "response.incomplete", "response.failed"}

var eventFrameSeeds = []string{
	"event: message_start\ndata: {\"a\":1}\n\nevent: content_block_delta\ndata: {\"t\":\"event: message_delta\"}\n\nevent:message_delta\ndata: {}\n\nevent: message_stop\ndata: {}",
	"\n\n\nevent: message_stop\ndata: {}\n\n\n\n",
	"data: {\"b\":2}\nevent: message_delta\n\nid: 7\nevent: message_delta\nevent: ping\ndata: x\n\n",
	"event:  message_delta\ndata: {}\n\nevent: message_delta \ndata: {}\n\n: event: message_stop\ndata: {}\n\n",
}

func TestEventFrames(t *testing.T) {
	bodies := slices.Clone(eventFrameSeeds)
	streams, err := filepath.Glob(filepath.Join("..", "..", "adapters", "testdata", "*", "*", "stream.sse"))
	if err != nil || len(streams) == 0 {
		t.Fatalf("no recorded streams: %v", err)
	}
	for _, path := range streams {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(b))
	}
	for _, body := range bodies {
		got, ok := sse.EventFrames([]byte(body), eventNames...)
		if !ok {
			t.Fatalf("EventFrames refused an LF stream: %q", body)
		}
		if want := splitEventFrames([]byte(body), eventNames...); !slices.EqualFunc(got, want, bytes.Equal) {
			t.Errorf("EventFrames(%q)\n got %q\nwant %q", body, got, want)
		}
	}
	if _, ok := sse.EventFrames([]byte("event: message_stop\r\ndata: {}\r\n\r\n"), eventNames...); ok {
		t.Error("EventFrames searched a CRLF stream")
	}
}

// FuzzEventFrames checks EventFrames against splitting every frame, on any LF stream.
func FuzzEventFrames(f *testing.F) {
	for _, s := range eventFrameSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, ok := sse.EventFrames(body, eventNames...)
		if !ok {
			return
		}
		if want := splitEventFrames(body, eventNames...); !slices.EqualFunc(got, want, bytes.Equal) {
			t.Fatalf("EventFrames(%q)\n got %q\nwant %q", body, got, want)
		}
	})
}
