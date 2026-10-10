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

// SplitSSEFrames is a bufio.SplitFunc yielding one SSE frame per token, per the WHATWG event-stream rules: a line ends in CRLF, LF or CR, and a blank line ends a frame. Blank lines before a frame are skipped, the token omits the blank line that ends it, and a frame unterminated at EOF is returned whole. Line endings inside the token are left as sent; NormalizeSSELineEnds rewrites them.
func SplitSSEFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0
	for start < len(data) && (data[start] == '\n' || data[start] == '\r') {
		start++
	}
	b := data[start:]
	if end, next := sseFrameEnd(b, atEOF); end >= 0 {
		return start + next, b[:end], nil
	}
	if !atEOF {
		return 0, nil, nil
	}
	if b = bytes.TrimRight(b, "\r\n"); len(b) == 0 {
		return len(data), nil, nil
	}
	return len(data), b, nil
}

// sseFrameEnd returns where the first frame in b ends and where the byte after its terminating blank line sits, or -1, -1 when b holds no blank line yet.
func sseFrameEnd(b []byte, atEOF bool) (end, next int) {
	lf := bytes.Index(b, []byte("\n\n"))
	head := b
	if lf >= 0 {
		head = b[:lf]
	}
	if bytes.IndexByte(head, '\r') < 0 {
		if lf < 0 {
			return -1, -1
		}
		return lf, lf + 2
	}
	for i := 0; ; {
		j := bytes.IndexAny(b[i:], "\r\n")
		if j < 0 {
			return -1, -1
		}
		end = i + j
		k := end + 1
		if b[end] == '\r' {
			// A trailing CR may be the first half of a CRLF not read yet.
			if k == len(b) && !atEOF {
				return -1, -1
			}
			if k < len(b) && b[k] == '\n' {
				k++
			}
		}
		if k == len(b) {
			return -1, -1
		}
		if b[k] == '\n' || b[k] == '\r' {
			// Waiting for a possible LF after a blank-line CR would hold every frame of a CR-only stream until the next one arrives; a late LF is skipped as a blank line before the next frame instead.
			next = k + 1
			if b[k] == '\r' && next < len(b) && b[next] == '\n' {
				next++
			}
			return end, next
		}
		i = k
	}
}

// NormalizeSSELineEnds rewrites each CRLF and lone CR in frame to LF, so a frame parses the same whichever line ending the stream used. frame itself is returned when it holds no CR.
func NormalizeSSELineEnds(frame []byte) []byte {
	i := bytes.IndexByte(frame, '\r')
	if i < 0 {
		return frame
	}
	out := make([]byte, i, len(frame))
	copy(out, frame[:i])
	for ; i < len(frame); i++ {
		c := frame[i]
		if c == '\r' {
			if i+1 < len(frame) && frame[i+1] == '\n' {
				continue
			}
			c = '\n'
		}
		out = append(out, c)
	}
	return out
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
