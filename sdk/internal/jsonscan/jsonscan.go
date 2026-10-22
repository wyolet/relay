// Package jsonscan walks the members of a JSON object and the elements of a
// JSON array in raw bytes without decoding them, for the vendor adapters'
// per-frame checks on stream hot paths. Keys compare the way encoding/json
// matches struct fields (case-insensitively); a key holding an escape
// sequence is reported as unreadable instead of being decoded. It does not
// validate: a scan of invalid JSON may succeed, so callers must only draw
// conclusions that hold when encoding/json rejects the same input.
package jsonscan

import "bytes"

// Members iterates the members of one JSON object.
type Members struct {
	rest       []byte
	key, value []byte
	started    bool
	done, bad  bool
}

// Object returns an iterator over the members of the object b (leading
// whitespace allowed). A b that is not an object ends the iteration with Err.
func Object(b []byte) Members { return Members{rest: b} }

// Next advances to the next member and reports whether there is one.
func (m *Members) Next() bool {
	if m.done || m.bad {
		return false
	}
	b := trimSpace(m.rest)
	if !m.opened(&b, '{', '}') {
		return false
	}
	if len(b) == 0 || b[0] != '"' {
		return m.fail()
	}
	n, ok := skipString(b)
	if !ok {
		return m.fail()
	}
	key := b[1 : n-1]
	if bytes.IndexByte(key, '\\') >= 0 {
		return m.fail()
	}
	b = trimSpace(b[n:])
	if len(b) == 0 || b[0] != ':' {
		return m.fail()
	}
	b = trimSpace(b[1:])
	vn, ok := skipValue(b)
	if !ok {
		return m.fail()
	}
	m.key, m.value, m.rest = key, b[:vn], b[vn:]
	return true
}

// opened consumes the opening bracket on the first call and the separator
// between members after it; false means the container ended or is malformed.
func (m *Members) opened(b *[]byte, open, close byte) bool {
	if !m.started {
		if len(*b) == 0 || (*b)[0] != open {
			return m.fail()
		}
		m.started = true
		*b = trimSpace((*b)[1:])
		if len(*b) > 0 && (*b)[0] == close {
			m.done = true
			return false
		}
		return true
	}
	switch {
	case len(*b) == 0:
		return m.fail()
	case (*b)[0] == close:
		m.done = true
		return false
	case (*b)[0] != ',':
		return m.fail()
	}
	*b = trimSpace((*b)[1:])
	return true
}

func (m *Members) fail() bool {
	m.bad = true
	return false
}

// Key returns the current member's key, without quotes.
func (m *Members) Key() []byte { return m.key }

// Value returns the current member's raw value.
func (m *Members) Value() []byte { return m.value }

// Err reports whether the iteration stopped on input it could not read.
func (m *Members) Err() bool { return m.bad }

// Rest returns the bytes after the last member returned (all of the input before any is).
func (m *Members) Rest() []byte { return m.rest }

// Elements iterates the elements of one JSON array.
type Elements struct{ m Members }

// Array returns an iterator over the elements of the array b (leading
// whitespace allowed). A b that is not an array ends the iteration with Err.
func Array(b []byte) Elements { return Elements{m: Members{rest: b}} }

// Next advances to the next element and reports whether there is one.
func (e *Elements) Next() bool {
	m := &e.m
	if m.done || m.bad {
		return false
	}
	b := trimSpace(m.rest)
	if !m.opened(&b, '[', ']') {
		return false
	}
	n, ok := skipValue(b)
	if !ok {
		return m.fail()
	}
	m.value, m.rest = b[:n], b[n:]
	return true
}

// Value returns the current element's raw value.
func (e *Elements) Value() []byte { return e.m.value }

// Err reports whether the iteration stopped on input it could not read.
func (e *Elements) Err() bool { return e.m.bad }

// KeyIs reports whether key names the field name the way encoding/json
// matches it.
func KeyIs(key []byte, name string) bool {
	return bytes.EqualFold(key, []byte(name))
}

// IsNull reports whether v is the literal null.
func IsNull(v []byte) bool { return string(v) == "null" }

// NullOrEmpty reports whether v is null or the empty string: the values a
// translator testing a string field for != "" treats as absent.
func NullOrEmpty(v []byte) bool { return string(v) == "null" || string(v) == `""` }

// StringIs reports whether v is a JSON string that spells s with no escape
// sequences. A string holding an escape reports false even when it decodes
// to s.
func StringIs(v []byte, s string) bool {
	return len(v) == len(s)+2 && v[0] == '"' && v[len(v)-1] == '"' &&
		string(v[1:len(v)-1]) == s && bytes.IndexByte(v, '\\') < 0
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	return b
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// skipString returns the length of the string starting at b[0] == '"'.
func skipString(b []byte) (int, bool) {
	for i := 1; ; {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return 0, false
		}
		i += j
		k := i - 1
		for k > 0 && b[k] == '\\' {
			k--
		}
		if (i-1-k)%2 == 0 {
			return i + 1, true
		}
		i++
	}
}

// skipValue returns the length of the value at the start of b. Nested
// brackets are counted, not matched by kind: valid JSON balances them.
func skipValue(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	switch b[0] {
	case '"':
		return skipString(b)
	case '{', '[':
		depth := 0
		for i := 0; i < len(b); {
			j := bytes.IndexAny(b[i:], `"{}[]`)
			if j < 0 {
				return 0, false
			}
			i += j
			switch b[i] {
			case '"':
				n, ok := skipString(b[i:])
				if !ok {
					return 0, false
				}
				i += n
				continue
			case '{', '[':
				depth++
			default:
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return 0, false
	default:
		i := 0
		for i < len(b) && !isSpace(b[i]) && b[i] != ',' && b[i] != '}' && b[i] != ']' {
			i++
		}
		return i, i > 0
	}
}
