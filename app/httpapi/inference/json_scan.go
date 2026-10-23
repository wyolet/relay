package inference

import (
	"encoding/binary"
	"math/bits"
)

// jsonMember is one member of a JSON object as byte offsets into the scanned buffer.
type jsonMember struct {
	keyStart, keyEnd int // the raw key, inside its quotes
	valStart, valEnd int
	name             string // the decoded key when the raw key holds an escape, "" otherwise
}

// jsonObject is a validated JSON value and, when it is an object, its members in order.
type jsonObject struct {
	b        []byte
	isObject bool
	open     int // index of the opening brace
	members  []jsonMember
}

var errJSONEnd = jsonSyntaxError("unexpected end of JSON input")

// scanJSONObject validates b as encoding/json does — the same grammar, string escapes, control characters and nesting limit, accepting invalid UTF-8 — and records the members of a top-level object. members is reused as the members' backing array.
func scanJSONObject(b []byte, members []jsonMember) (jsonObject, *bodyError) {
	obj := jsonObject{b: b, members: members[:0]}
	s := jsonScanner{b: b}
	i := skipSpace(b, 0)
	if i == len(b) {
		return obj, errJSONEnd
	}
	if b[i] != '{' {
		end, err := s.value(i)
		if err != nil {
			return obj, err
		}
		if j := skipSpace(b, end); j < len(b) {
			return obj, syntaxErrorAt(b, j, "after top-level value")
		}
		return obj, nil
	}
	obj.isObject, obj.open = true, i
	s.depth = 1
	i = skipSpace(b, i+1)
	if i < len(b) && b[i] == '}' {
		return obj, s.trailing(i + 1)
	}
	for {
		if i >= len(b) {
			return obj, errJSONEnd
		}
		if b[i] != '"' {
			return obj, syntaxErrorAt(b, i, "looking for beginning of object key string")
		}
		m := jsonMember{keyStart: i + 1}
		end, escaped, err := readJSONString(b, i)
		if err != nil {
			return obj, err
		}
		m.keyEnd = end - 1
		if escaped {
			if m.name, err = decodeJSONString(b[m.keyStart:m.keyEnd]); err != nil {
				return obj, err
			}
		}
		if i, err = colon(b, end); err != nil {
			return obj, err
		}
		m.valStart = skipSpace(b, i)
		if m.valEnd, err = s.value(m.valStart); err != nil {
			return obj, err
		}
		obj.members = append(obj.members, m)
		i = skipSpace(b, m.valEnd)
		if i >= len(b) {
			return obj, errJSONEnd
		}
		switch b[i] {
		case ',':
			i = skipSpace(b, i+1)
		case '}':
			return obj, s.trailing(i + 1)
		default:
			return obj, syntaxErrorAt(b, i, "after object key:value pair")
		}
	}
}

func syntaxErrorAt(b []byte, i int, context string) *bodyError {
	return jsonSyntaxError("invalid character %q %s at offset %d", b[i], context, i)
}

// jsonScanner validates nested values; it tracks whether each open container is an array.
type jsonScanner struct {
	b      []byte
	depth  int
	arrays uint64   // bit i: container at depth i+1 is an array (first 64 levels)
	deeper []uint64 // the same past 64 levels
}

// trailing accepts only whitespace from i to the end.
func (s *jsonScanner) trailing(i int) *bodyError {
	if j := skipSpace(s.b, i); j < len(s.b) {
		return syntaxErrorAt(s.b, j, "after top-level value")
	}
	return nil
}

func (s *jsonScanner) push(array bool) *bodyError {
	if s.depth == maxScanDepth {
		return jsonSyntaxError("exceeded max depth")
	}
	if d := s.depth; d < 64 {
		if array {
			s.arrays |= 1 << d
		} else {
			s.arrays &^= 1 << d
		}
	} else {
		w, bit := (d-64)/64, uint((d-64)%64)
		for len(s.deeper) <= w {
			s.deeper = append(s.deeper, 0)
		}
		if array {
			s.deeper[w] |= 1 << bit
		} else {
			s.deeper[w] &^= 1 << bit
		}
	}
	s.depth++
	return nil
}

func (s *jsonScanner) inArray() bool {
	d := s.depth - 1
	if d < 64 {
		return s.arrays&(1<<d) != 0
	}
	return s.deeper[(d-64)/64]&(1<<uint((d-64)%64)) != 0
}

// value validates the value starting at i, containers included, and returns the index after it.
func (s *jsonScanner) value(i int) (int, *bodyError) {
	b := s.b
	base := s.depth
	var err *bodyError
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return i, errJSONEnd
		}
		switch c := b[i]; {
		case c == '"':
			if i, _, err = readJSONString(b, i); err != nil {
				return i, err
			}
		case c == '{' || c == '[':
			if err = s.push(c == '['); err != nil {
				return i, err
			}
			i = skipSpace(b, i+1)
			if i < len(b) && (b[i] == '}' && c == '{' || b[i] == ']' && c == '[') {
				s.depth--
				i++
				break
			}
			if c == '{' {
				if i, err = s.key(i); err != nil {
					return i, err
				}
			}
			continue
		case c == '-' || c >= '0' && c <= '9':
			if i, err = readJSONNumber(b, i); err != nil {
				return i, err
			}
		case c == 't':
			if i, err = readJSONLiteral(b, i, "true"); err != nil {
				return i, err
			}
		case c == 'f':
			if i, err = readJSONLiteral(b, i, "false"); err != nil {
				return i, err
			}
		case c == 'n':
			if i, err = readJSONLiteral(b, i, "null"); err != nil {
				return i, err
			}
		default:
			return i, syntaxErrorAt(b, i, "looking for beginning of value")
		}
		// A value ended: close what it completes, then move on to the next element.
		for next := false; !next; {
			if s.depth == base {
				return i, nil
			}
			i = skipSpace(b, i)
			if i >= len(b) {
				return i, errJSONEnd
			}
			inArray := s.inArray()
			switch c := b[i]; {
			case c == ',':
				i++
				if !inArray {
					if i, err = s.key(skipSpace(b, i)); err != nil {
						return i, err
					}
				}
				next = true
			case c == ']' && inArray, c == '}' && !inArray:
				s.depth--
				i++
			case inArray:
				return i, syntaxErrorAt(b, i, "after array element")
			default:
				return i, syntaxErrorAt(b, i, "after object key:value pair")
			}
		}
	}
}

// key validates an object key at i and the colon after it, returning the index after the colon.
func (s *jsonScanner) key(i int) (int, *bodyError) {
	b := s.b
	if i >= len(b) {
		return i, errJSONEnd
	}
	if b[i] != '"' {
		return i, syntaxErrorAt(b, i, "looking for beginning of object key string")
	}
	end, _, err := readJSONString(b, i)
	if err != nil {
		return end, err
	}
	return colon(b, end)
}

func colon(b []byte, i int) (int, *bodyError) {
	i = skipSpace(b, i)
	if i >= len(b) {
		return i, errJSONEnd
	}
	if b[i] != ':' {
		return i, syntaxErrorAt(b, i, "after object key")
	}
	return i + 1, nil
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		if c := b[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			break
		}
		i++
	}
	return i
}

const (
	swarBackslash = '\\' * swarOnes
	swarControl   = 0x20 * swarOnes
)

// readJSONString validates the string starting at b[i] == '"' and returns the index after its closing quote and whether it holds an escape.
//
// It finds the next '"', '\\' or control character eight bytes per step; each mask is exact for its lowest flagged byte, so their union's lowest is the first such byte.
func readJSONString(b []byte, i int) (end int, escaped bool, err *bodyError) {
	i++
	for {
		for ; i+8 <= len(b); i += 8 {
			x := binary.LittleEndian.Uint64(b[i:])
			q := x ^ swarQuote
			bs := x ^ swarBackslash
			if m := (((q - swarOnes) &^ q) | ((bs - swarOnes) &^ bs) | ((x - swarControl) &^ x)) & swarHigh; m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				break
			}
		}
		for ; i < len(b); i++ {
			if c := b[i]; c == '"' || c == '\\' || c < 0x20 {
				break
			}
		}
		if i >= len(b) {
			return i, escaped, errJSONEnd
		}
		switch c := b[i]; c {
		case '"':
			return i + 1, escaped, nil
		case '\\':
			escaped = true
			if i+1 >= len(b) {
				return i, escaped, errJSONEnd
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				for k := i + 2; k < i+6; k++ {
					if k >= len(b) {
						return k, escaped, errJSONEnd
					}
					if !isHex(b[k]) {
						return k, escaped, syntaxErrorAt(b, k, "in \\u hexadecimal character escape")
					}
				}
				i += 6
			default:
				return i, escaped, syntaxErrorAt(b, i+1, "in string escape code")
			}
		default:
			return i, escaped, syntaxErrorAt(b, i, "in string literal")
		}
	}
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// readJSONNumber validates the number at i against JSON's grammar and returns the index after it.
func readJSONNumber(b []byte, i int) (int, *bodyError) {
	if b[i] == '-' {
		i++
	}
	if i >= len(b) {
		return i, errJSONEnd
	}
	switch {
	case b[i] == '0':
		i++
	case isDigit(b[i]):
		for i++; i < len(b) && isDigit(b[i]); i++ {
		}
	default:
		return i, syntaxErrorAt(b, i, "in numeric literal")
	}
	if i < len(b) && b[i] == '.' {
		if i++; i >= len(b) {
			return i, errJSONEnd
		}
		if !isDigit(b[i]) {
			return i, syntaxErrorAt(b, i, "after decimal point in numeric literal")
		}
		for i++; i < len(b) && isDigit(b[i]); i++ {
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		if i++; i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= len(b) {
			return i, errJSONEnd
		}
		if !isDigit(b[i]) {
			return i, syntaxErrorAt(b, i, "in exponent of numeric literal")
		}
		for i++; i < len(b) && isDigit(b[i]); i++ {
		}
	}
	return i, nil
}

func readJSONLiteral(b []byte, i int, lit string) (int, *bodyError) {
	for k := 0; k < len(lit); k++ {
		if i+k >= len(b) {
			return i + k, errJSONEnd
		}
		if b[i+k] != lit[k] {
			return i + k, syntaxErrorAt(b, i+k, "in literal "+lit)
		}
	}
	return i + len(lit), nil
}
