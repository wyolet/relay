package dedup

import (
	"bytes"
	"encoding/json"
)

// member is one top-level key of a JSON object and its value's byte span.
type member struct {
	key                  string
	valueStart, valueEnd int
}

// topLevelMembers returns the members of a JSON object body, or false when
// body is not a valid JSON object.
func topLevelMembers(body []byte) ([]member, bool) {
	if !json.Valid(body) {
		return nil, false
	}
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, false
	}
	i = skipSpace(body, i+1)
	var out []member
	for i < len(body) && body[i] != '}' {
		keyEnd := skipString(body, i)
		key, ok := decodeKey(body[i:keyEnd])
		if !ok {
			return nil, false
		}
		i = skipSpace(body, keyEnd) // at ':'
		i = skipSpace(body, i+1)
		end := skipValue(body, i)
		out = append(out, member{key: key, valueStart: i, valueEnd: end})
		i = skipSpace(body, end)
		if i < len(body) && body[i] == ',' {
			i = skipSpace(body, i+1)
		}
	}
	return out, true
}

// arrayElements returns the [start, end) span of each element of the array
// at body[start:end]. body must already be valid JSON.
func arrayElements(body []byte, start, end int) ([][2]int, bool) {
	i := skipSpace(body, start+1)
	var out [][2]int
	for i < end && body[i] != ']' {
		e := skipValue(body, i)
		out = append(out, [2]int{i, e})
		i = skipSpace(body, e)
		if i < end && body[i] == ',' {
			i = skipSpace(body, i+1)
		}
	}
	return out, i < end
}

func decodeKey(raw []byte) (string, bool) {
	if !bytes.ContainsRune(raw, '\\') {
		return string(raw[1 : len(raw)-1]), true
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index just past the string starting at b[i] == '"'.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index just past the value starting at b[i]. b is
// valid JSON, so only strings and bracket depth need tracking.
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				i = skipString(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return i
	default:
		for ; i < len(b); i++ {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
		}
		return i
	}
}
