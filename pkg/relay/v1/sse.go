package v1

import "bytes"

// SSEFrame is one server-sent event ready for the wire: an event name plus
// the JSON-marshaled event payload.
type SSEFrame struct {
	Event string // one of the Event* constants in events.go
	Data  []byte // JSON-marshaled event payload
}

// Bytes serializes the frame to its on-wire SSE form:
//
//	event: <name>\ndata: <json>\n\n
func (f SSEFrame) Bytes() []byte {
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

// ParseSSEChunk extracts event and data from a raw SSE chunk (one frame,
// the bytes between two blank-line separators).
func ParseSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	lines := bytes.Split(bytes.TrimRight(chunk, "\n"), []byte("\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("event:")) {
			event = string(bytes.TrimSpace(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(line[5:])
		}
	}
	return event, data, len(data) > 0
}
