package openai

import (
	"bytes"

	"github.com/wyolet/relay/sdk/internal/sse"
)

// ResponsesSSEFrame is one Responses API server-sent event ready for the wire:
// an event name plus the JSON-marshaled event payload. Both fields are required
// for Responses stream events (every Responses event carries an explicit `event:` line).
type ResponsesSSEFrame struct {
	Event string // one of the ResponsesEvent* constants in responses_events.go
	Data  []byte // JSON-marshaled event payload
}

// Bytes serializes the frame to its on-wire SSE form:
//
//	event: <name>\ndata: <json>\n\n
func (f ResponsesSSEFrame) Bytes() []byte {
	var b bytes.Buffer
	if f.Event != "" {
		b.WriteString("event: ")
		b.WriteString(f.Event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(f.Data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// ParseResponsesSSEChunk extracts event and data from a raw SSE chunk (one frame,
// the bytes between two blank-line separators with the trailing \n\n
// reattached by the caller). Multi-line data is joined with "\n". A chunk
// holding several events yields the first, since a frame ends at its first blank line.
func ParseResponsesSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	sc := sse.NewScanner(chunk)
	if !sc.Next() {
		return "", nil, false
	}
	return string(sc.Event()), sc.Data(), true
}
