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
