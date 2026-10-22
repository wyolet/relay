// Package sse splits server-sent-event bytes into events per the WHATWG
// event-stream rules, for the vendor adapters' to-canonical stream parsers and
// usage walkers: events end at a blank line, an event's data is its data lines
// joined with "\n", lines end in LF, CRLF or CR, and comments and unknown
// fields are ignored. It is vendor-neutral and keeps no state across inputs;
// reconnection (id, retry) and splitting a byte stream into chunks are out of
// scope.
package sse

import "bytes"

// Scanner walks the events of one SSE byte slice. Event and Data alias the
// input, except Data of a multi-line event, which is a fresh slice.
type Scanner struct {
	rest  []byte
	event []byte
	data  []byte
}

// NewScanner returns a Scanner over b.
func NewScanner(b []byte) Scanner {
	return Scanner{rest: b}
}

// Next advances to the next event and reports whether there is one. Events
// without data are skipped: they carry no payload to translate. The input's
// end closes a pending event, since callers hand over chunks with the blank
// line already stripped.
func (s *Scanner) Next() bool {
	s.event, s.data = nil, nil
	dataLines := 0
	for len(s.rest) > 0 {
		var line []byte
		line, s.rest = nextLine(s.rest)
		if len(line) == 0 {
			if len(s.data) > 0 {
				return true
			}
			s.event, s.data, dataLines = nil, nil, 0
			continue
		}
		field, value := splitField(line)
		switch string(field) {
		case "event":
			s.event = value
		case "data":
			switch dataLines {
			case 0:
				s.data = value
			case 1:
				// Copy before joining: appending to the alias would write into the input.
				joined := make([]byte, 0, len(s.data)+1+len(value))
				joined = append(joined, s.data...)
				joined = append(joined, '\n')
				s.data = append(joined, value...)
			default:
				s.data = append(append(s.data, '\n'), value...)
			}
			dataLines++
		}
	}
	return len(s.data) > 0
}

// Event returns the current event's type, empty when it named none.
func (s *Scanner) Event() []byte { return s.event }

// Data returns the current event's data.
func (s *Scanner) Data() []byte { return s.data }

func nextLine(b []byte) (line, rest []byte) {
	lf := bytes.IndexByte(b, '\n')
	end := lf
	if end < 0 {
		end = len(b)
	}
	if cr := bytes.IndexByte(b[:end], '\r'); cr >= 0 {
		if cr+1 < len(b) && b[cr+1] == '\n' {
			return b[:cr], b[cr+2:]
		}
		return b[:cr], b[cr+1:]
	}
	if lf < 0 {
		return b, nil
	}
	return b[:lf], b[lf+1:]
}

func splitField(line []byte) (field, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return line[:i], value
}
