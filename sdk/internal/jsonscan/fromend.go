package jsonscan

import "bytes"

// EndsWindow is how far into a buffered response, from either end, the vendor adapters' ends readers look for its Summary.
const EndsWindow = 64 << 10

// MembersFromEnd iterates the members of one JSON object from the last to the first, so the members a shape puts at the end of a large body are reached without reading what comes before them.
type MembersFromEnd struct {
	b          []byte
	floor      int
	pos        int
	key, value []byte
	valueEnd   int
	started    bool
	done, bad  bool
}

// ObjectFromEnd returns an iterator over the members of the object b ends with (trailing whitespace allowed), last member first. No byte before b[floor] is read: a member reaching below it ends the iteration with Err.
func ObjectFromEnd(b []byte, floor int) MembersFromEnd {
	return MembersFromEnd{b: b, floor: max(floor, 0), pos: len(b)}
}

// Next steps back to the previous member and reports whether there is one.
func (m *MembersFromEnd) Next() bool {
	i, ok := m.stepBack('{', '}')
	if !ok {
		return false
	}
	vs, ok := skipValueBack(m.b, i, m.floor)
	if !ok {
		return m.fail()
	}
	j := m.trimSpaceBack(vs)
	if j <= m.floor || m.b[j-1] != ':' {
		return m.fail()
	}
	j = m.trimSpaceBack(j - 1)
	if j <= m.floor || m.b[j-1] != '"' {
		return m.fail()
	}
	ks, ok := skipStringBack(m.b, j, m.floor)
	if !ok {
		return m.fail()
	}
	key := m.b[ks+1 : j-1]
	if bytes.IndexByte(key, '\\') >= 0 {
		return m.fail()
	}
	m.key, m.value, m.valueEnd, m.pos = key, m.b[vs:i], i, ks
	return true
}

// stepBack consumes the closing bracket on the first call and the separator before a member after it, returning where the next value ends; false means the container began or is malformed.
func (m *MembersFromEnd) stepBack(open, close byte) (int, bool) {
	if m.done || m.bad {
		return 0, false
	}
	i := m.trimSpaceBack(m.pos)
	if i <= m.floor {
		return 0, m.fail()
	}
	if !m.started {
		if m.b[i-1] != close {
			return 0, m.fail()
		}
		m.started = true
		i = m.trimSpaceBack(i - 1)
		if i <= m.floor {
			return 0, m.fail()
		}
		if m.b[i-1] == open {
			m.pos, m.done = i-1, true
			return 0, false
		}
		return i, true
	}
	switch m.b[i-1] {
	case open:
		m.pos, m.done = i-1, true
		return 0, false
	case ',':
		return m.trimSpaceBack(i - 1), true
	}
	return 0, m.fail()
}

func (m *MembersFromEnd) fail() bool {
	m.bad = true
	return false
}

func (m *MembersFromEnd) trimSpaceBack(i int) int {
	for i > m.floor && isSpace(m.b[i-1]) {
		i--
	}
	return i
}

// Key returns the current member's key, without quotes.
func (m *MembersFromEnd) Key() []byte { return m.key }

// Value returns the current member's raw value.
func (m *MembersFromEnd) Value() []byte { return m.value }

// Err reports whether the iteration stopped at the floor or on input it could not read, so earlier members may exist unseen.
func (m *MembersFromEnd) Err() bool { return m.bad }

// Unread returns the bytes before the last member returned (all of b before any is): what the iteration has not walked.
func (m *MembersFromEnd) Unread() []byte { return m.b[:m.pos] }

// ValueObject returns an iterator over the current value's members, with the same floor.
func (m *MembersFromEnd) ValueObject() MembersFromEnd {
	return ObjectFromEnd(m.b[:m.valueEnd], m.floor)
}

// ValueArray returns an iterator over the current value's elements, with the same floor.
func (m *MembersFromEnd) ValueArray() ElementsFromEnd { return ArrayFromEnd(m.b[:m.valueEnd], m.floor) }

// ElementsFromEnd iterates the elements of one JSON array from the last to the first.
type ElementsFromEnd struct{ m MembersFromEnd }

// ArrayFromEnd returns an iterator over the elements of the array b ends with (trailing whitespace allowed), last element first, reading no byte before b[floor].
func ArrayFromEnd(b []byte, floor int) ElementsFromEnd {
	return ElementsFromEnd{m: MembersFromEnd{b: b, floor: max(floor, 0), pos: len(b)}}
}

// Next steps back to the previous element and reports whether there is one.
func (e *ElementsFromEnd) Next() bool {
	m := &e.m
	i, ok := m.stepBack('[', ']')
	if !ok {
		return false
	}
	vs, ok := skipValueBack(m.b, i, m.floor)
	if !ok {
		return m.fail()
	}
	m.value, m.valueEnd, m.pos = m.b[vs:i], i, vs
	return true
}

// Value returns the current element's raw value.
func (e *ElementsFromEnd) Value() []byte { return e.m.value }

// Err reports whether the iteration stopped at the floor or on input it could not read.
func (e *ElementsFromEnd) Err() bool { return e.m.bad }

// Unread returns the bytes before the last element returned.
func (e *ElementsFromEnd) Unread() []byte { return e.m.Unread() }

// ValueObject returns an iterator over the current element's members, with the same floor.
func (e *ElementsFromEnd) ValueObject() MembersFromEnd { return e.m.ValueObject() }

// ValueArray returns an iterator over the current element's elements, with the same floor.
func (e *ElementsFromEnd) ValueArray() ElementsFromEnd { return e.m.ValueArray() }

// skipStringBack returns where the string ending at b[end-1] == '"' starts. A quote inside a string is always escaped, i.e. preceded by an odd run of backslashes, so the first quote back with an even run opens it.
func skipStringBack(b []byte, end, floor int) (int, bool) {
	for i := end - 1; i > floor; {
		j := bytes.LastIndexByte(b[floor:i], '"')
		if j < 0 {
			return 0, false
		}
		j += floor
		k := j
		for k > floor && b[k-1] == '\\' {
			k--
		}
		if k == floor && floor > 0 && k < j {
			// The backslash run may continue below the floor.
			return 0, false
		}
		if (j-k)%2 == 0 {
			return j, true
		}
		i = j
	}
	return 0, false
}

// skipValueBack returns where the value ending at b[end-1] starts. Nested brackets are counted, not matched by kind: valid JSON balances them.
func skipValueBack(b []byte, end, floor int) (int, bool) {
	if end <= floor {
		return 0, false
	}
	switch b[end-1] {
	case '"':
		return skipStringBack(b, end, floor)
	case '}', ']':
		depth := 0
		for i := end - 1; i >= floor; i-- {
			switch b[i] {
			case '"':
				s, ok := skipStringBack(b, i+1, floor)
				if !ok {
					return 0, false
				}
				i = s
			case '}', ']':
				depth++
			case '{', '[':
				depth--
				if depth == 0 {
					return i, true
				}
			}
		}
		return 0, false
	default:
		i := end
		for i > floor && !isSpace(b[i-1]) && b[i-1] != ',' && b[i-1] != ':' && b[i-1] != '[' && b[i-1] != '{' {
			i--
		}
		if i == floor && floor > 0 {
			return 0, false
		}
		return i, i < end
	}
}

// foldsToS and foldsToK are U+017F and U+212A in UTF-8.
var foldsToS, foldsToK = []byte{0xc5, 0xbf}, []byte{0xe2, 0x84, 0xaa}

// MayHoldString reports whether b could hold one of the ASCII values as a JSON string spelled without escapes, wherever it sits, compared the way encoding/json matches keys: case-folded, and true whenever b holds U+017F or U+212A, which fold onto s and k from outside ASCII. One pass over the quotes, where a bytes.Index per value would rescan dense JSON once each.
func MayHoldString(b []byte, values ...string) bool {
	if bytes.Contains(b, foldsToS) || bytes.Contains(b, foldsToK) {
		return true
	}
	for i := 0; ; i++ {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return false
		}
		i += j
		for _, v := range values {
			if end := i + 1 + len(v); end < len(b) && b[end] == '"' && bytes.EqualFold(b[i+1:end], []byte(v)) {
				return true
			}
		}
	}
}

// HasLetterEscape reports whether b holds a \u escape of an ASCII character from @ to DEL (the letters, the underscore and the punctuation between them): one could spell a key or a value a byte search for its plain form misses. Escapes of control characters, quotes, digits and <, >, & do not count.
func HasLetterEscape(b []byte) bool {
	for i := 0; i < len(b); i += 2 {
		j := bytes.IndexByte(b[i:], '\\')
		if j < 0 {
			return false
		}
		i += j
		if i+4 < len(b) && b[i+1] == 'u' && b[i+2] == '0' && b[i+3] == '0' && b[i+4] >= '4' && b[i+4] <= '7' {
			return true
		}
	}
	return false
}
