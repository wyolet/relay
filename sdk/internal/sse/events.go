package sse

import "bytes"

// EventFrames returns, in stream order, the events of b whose type is one of names, each as the bytes v1.SplitSSEFrames would cut for it, found from the event lines alone instead of splitting every event. ok is false when b holds a CR: only LF-terminated streams are searched, since a CR line end would make the blank-line search ambiguous.
func EventFrames(b []byte, names ...string) (frames [][]byte, ok bool) {
	if bytes.IndexByte(b, '\r') >= 0 {
		return nil, false
	}
	type span struct{ start, end int }
	var spans []span
	for line, first := 0, true; ; first = false {
		// Searching from the previous event line's start finds the line ending it, so back-to-back event lines are both seen.
		if !first || !bytes.HasPrefix(b, []byte("event:")) {
			k := bytes.Index(b[line:], []byte("\nevent:"))
			if k < 0 {
				break
			}
			line += k + 1
		}
		next := len(b)
		if k := bytes.IndexByte(b[line:], '\n'); k >= 0 {
			next = line + k + 1
		}
		if value := bytes.TrimSuffix(b[line+len("event:"):next], []byte("\n")); oneOf(bytes.TrimPrefix(value, []byte(" ")), names) {
			s := span{start: 0, end: len(b)}
			if k := bytes.LastIndex(b[:line], []byte("\n\n")); k >= 0 {
				s.start = k + 2
			}
			for s.start < line && b[s.start] == '\n' {
				s.start++
			}
			if k := bytes.Index(b[line:], []byte("\n\n")); k >= 0 {
				s.end = line + k
			}
			if len(spans) == 0 || spans[len(spans)-1] != s {
				spans = append(spans, s)
			}
		}
	}
	frames = make([][]byte, 0, len(spans))
	for _, s := range spans {
		if frame := bytes.TrimRight(b[s.start:s.end], "\n"); namesEvent(frame, names) {
			frames = append(frames, frame)
		}
	}
	return frames, true
}

// namesEvent reports whether frame is one event, with data, whose type (its last event line) is one of names: an event without data reaches no translator, and an earlier event line is overridden.
func namesEvent(frame []byte, names []string) bool {
	sc := NewScanner(frame)
	return sc.Next() && oneOf(sc.Event(), names)
}

func oneOf(v []byte, names []string) bool {
	for _, n := range names {
		if string(v) == n {
			return true
		}
	}
	return false
}
