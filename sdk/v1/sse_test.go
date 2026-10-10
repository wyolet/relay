package v1

import (
	"bytes"
	"testing"
)

func TestSSEFrameBytes(t *testing.T) {
	f := SSEFrame{
		Event: "item.delta",
		Data:  []byte(`{"item_id":"i1","delta":"hello"}`),
	}
	got := f.Bytes()
	want := "event: item.delta\ndata: {\"item_id\":\"i1\",\"delta\":\"hello\"}\n\n"
	if string(got) != want {
		t.Errorf("Bytes:\ngot  %q\nwant %q", got, want)
	}
}

func TestSSEFrameBytesNoEvent(t *testing.T) {
	f := SSEFrame{
		Data: []byte(`{"hello":"world"}`),
	}
	got := f.Bytes()
	want := "data: {\"hello\":\"world\"}\n\n"
	if string(got) != want {
		t.Errorf("Bytes:\ngot  %q\nwant %q", got, want)
	}
}

func TestParseSSEChunk(t *testing.T) {
	tests := []struct {
		name      string
		chunk     []byte
		wantEvent string
		wantData  string
		wantOK    bool
	}{
		{
			name:      "standard event+data",
			chunk:     []byte("event: item.delta\ndata: {\"delta\":\"hi\"}\n\n"),
			wantEvent: "item.delta",
			wantData:  `{"delta":"hi"}`,
			wantOK:    true,
		},
		{
			name:      "data only",
			chunk:     []byte("data: {\"x\":1}\n\n"),
			wantEvent: "",
			wantData:  `{"x":1}`,
			wantOK:    true,
		},
		{
			name:      "empty data",
			chunk:     []byte("event: ping\n\n"),
			wantEvent: "ping",
			wantData:  "",
			wantOK:    false,
		},
		{
			name:      "empty data line",
			chunk:     []byte("event: ping\ndata:\n\n"),
			wantEvent: "ping",
			wantData:  "",
			wantOK:    false,
		},
		{
			name:      "multi-line data joins with LF",
			chunk:     []byte("event: e\ndata: {\"a\":\ndata: 1}\ndata: x\n\n"),
			wantEvent: "e",
			wantData:  "{\"a\":\n1}\nx",
			wantOK:    true,
		},
		{
			name:      "CRLF line endings",
			chunk:     []byte("event: e\r\ndata: a\r\ndata: b\r\n\r\n"),
			wantEvent: "e",
			wantData:  "a\nb",
			wantOK:    true,
		},
		{
			name:      "CR line endings",
			chunk:     []byte("event: e\rdata: a\rdata: b\r\r"),
			wantEvent: "e",
			wantData:  "a\nb",
			wantOK:    true,
		},
		{
			name:      "comment lines ignored",
			chunk:     []byte(": keepalive\nevent: e\n:data: no\ndata: x\n\n"),
			wantEvent: "e",
			wantData:  "x",
			wantOK:    true,
		},
		{
			name:      "strips exactly one leading space",
			chunk:     []byte("event:  e\ndata:  x \n\n"),
			wantEvent: " e",
			wantData:  " x ",
			wantOK:    true,
		},
		{
			name:      "no space after colon",
			chunk:     []byte("event:e\ndata:x\n\n"),
			wantEvent: "e",
			wantData:  "x",
			wantOK:    true,
		},
		{
			name:      "last event line wins",
			chunk:     []byte("event: a\ndata: x\nevent: b\n\n"),
			wantEvent: "b",
			wantData:  "x",
			wantOK:    true,
		},
		{
			name:      "first frame with data is parsed",
			chunk:     []byte("event: a\n\nevent: b\ndata: x\n\nevent: c\ndata: y\n\n"),
			wantEvent: "b",
			wantData:  "x",
			wantOK:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, data, ok := ParseSSEChunk(tc.chunk)
			if event != tc.wantEvent {
				t.Errorf("event: got %q, want %q", event, tc.wantEvent)
			}
			if string(data) != tc.wantData {
				t.Errorf("data: got %q, want %q", data, tc.wantData)
			}
			if ok != tc.wantOK {
				t.Errorf("ok: got %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestParseSSEChunkMultiLineLeavesChunkIntact(t *testing.T) {
	chunk := []byte("data: a\ndata: b\ndata: c\n\n")
	want := string(chunk)
	if _, data, _ := ParseSSEChunk(chunk); string(data) != "a\nb\nc" {
		t.Fatalf("data: got %q", data)
	}
	if string(chunk) != want {
		t.Errorf("chunk modified: got %q, want %q", chunk, want)
	}
}

func TestParseSSEChunkAllocs(t *testing.T) {
	dataOnly := []byte("data: {\"x\":1}\n\n")
	if n := testing.AllocsPerRun(100, func() { ParseSSEChunk(dataOnly) }); n != 0 {
		t.Errorf("data-only frame: %v allocs, want 0", n)
	}
	// The one allocation is the event name's string conversion.
	withEvent := []byte("event: item.delta\ndata: {\"x\":1}\n\n")
	if n := testing.AllocsPerRun(100, func() { ParseSSEChunk(withEvent) }); n != 1 {
		t.Errorf("event+data frame: %v allocs, want 1", n)
	}
}

func TestSSEFrameRoundTrip(t *testing.T) {
	f := SSEFrame{
		Event: EventItemDelta,
		Data:  []byte(`{"item_id":"x","kind":"text","delta":"chunk"}`),
	}
	wire := f.Bytes()
	event, data, ok := ParseSSEChunk(wire)
	if !ok {
		t.Fatal("ParseSSEChunk: ok=false")
	}
	if event != f.Event {
		t.Errorf("event: got %q, want %q", event, f.Event)
	}
	if !bytes.Equal(data, f.Data) {
		t.Errorf("data: got %q, want %q", data, f.Data)
	}
}
