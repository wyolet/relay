package jsonscan

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/bits"
	"strconv"
	"unicode/utf8"
)

// Reader reads one JSON text in a single pass that checks its syntax: Object
// and Array walk containers, Value returns a value's raw bytes, and every
// value the caller leaves unread is skipped, checked but not decoded. It is
// sound, not complete: a text it reads to End without Err is one
// encoding/json accepts, while some valid texts (an escaped key, nesting
// deeper than maxDepth) stop it with Err, and callers then fall back to
// encoding/json.
type Reader struct {
	b     []byte
	i     int
	depth int
	bad   bool
}

// maxDepth bounds container nesting, below encoding/json's own limit.
const maxDepth = 1000

// Read returns a Reader positioned at the start of b.
func Read(b []byte) Reader { return Reader{b: b} }

// Err reports whether the Reader stopped on input it could not read.
func (r *Reader) Err() bool { return r.bad }

// End reports whether the text was read without Err and only whitespace
// follows it.
func (r *Reader) End() bool {
	r.skipSpace()
	return !r.bad && r.i == len(r.b)
}

// Object reads the object at the cursor, calling member with each key (as in
// Members) and the cursor at its value, which member may read with Object,
// Array or Value at most once. It returns false, consuming nothing, when the
// value at the cursor is not an object.
func (r *Reader) Object(member func(key []byte)) bool {
	if !r.open('{') {
		return false
	}
	if r.close('}') {
		return true
	}
	for {
		key, ok := r.key()
		if !ok {
			return r.fail()
		}
		r.skipSpace()
		if r.i >= len(r.b) || r.b[r.i] != ':' {
			return r.fail()
		}
		r.i++
		r.skipSpace()
		start := r.i
		member(key)
		r.skipUnread(start)
		if r.bad || r.close('}') {
			return true
		}
		if !r.comma() {
			return r.fail()
		}
	}
}

// Array reads the array at the cursor, calling elem with each element's index
// and the cursor at the element, which elem may read at most once. It returns
// false, consuming nothing, when the value at the cursor is not an array.
func (r *Reader) Array(elem func(i int)) bool {
	if !r.open('[') {
		return false
	}
	if r.close(']') {
		return true
	}
	for i := 0; ; i++ {
		r.skipSpace()
		start := r.i
		elem(i)
		r.skipUnread(start)
		if r.bad || r.close(']') {
			return true
		}
		if !r.comma() {
			return r.fail()
		}
	}
}

// Value reads the value at the cursor and returns its raw bytes; nil after
// Err.
func (r *Reader) Value() []byte {
	if r.bad {
		return nil
	}
	r.skipSpace()
	start := r.i
	r.skipValue()
	if r.bad {
		return nil
	}
	return r.b[start:r.i]
}

// StringField reads the value at the cursor the way encoding/json decodes it
// into a string field: a string replaces *raw with its raw bytes (String
// decodes them), null leaves *raw as is, and any other value returns false.
func (r *Reader) StringField(raw *[]byte) bool {
	switch v := r.Value(); {
	case len(v) > 0 && v[0] == '"':
		*raw = v
		return true
	case IsNull(v):
		return true
	}
	return false
}

// StructField reads the value at the cursor the way encoding/json decodes it
// into a struct field: an object member by member, as Object does, or null.
// It returns false for any other value and once a member returns false,
// skipping the members after it.
func (r *Reader) StructField(member func(key []byte) bool) bool {
	ok := true
	if r.Object(func(key []byte) { ok = ok && member(key) }) {
		return ok
	}
	return IsNull(r.Value())
}

// SliceField reads the value at the cursor the way encoding/json decodes it
// into a slice field: an array element by element, as Array does, or null.
// It returns false for any other value and once an element returns false,
// skipping the elements after it.
func (r *Reader) SliceField(elem func(i int) bool) bool {
	ok := true
	if r.Array(func(i int) { ok = ok && elem(i) }) {
		return ok
	}
	return IsNull(r.Value())
}

// IntField reads the value at the cursor the way encoding/json decodes it into
// an int field: an integer replaces *n, null leaves it, and any other value
// (a fraction, an exponent, an overflow, a non-number) returns false.
func (r *Reader) IntField(n *int) bool {
	v := r.Value()
	if IsNull(v) {
		return true
	}
	if len(v) == 0 || (v[0] != '-' && !isDigit(v[0])) {
		return false
	}
	x, err := strconv.ParseInt(string(v), 10, strconv.IntSize)
	if err != nil {
		return false
	}
	*n = int(x)
	return true
}

// skipUnread skips the value at start when the caller's callback left it
// unread.
func (r *Reader) skipUnread(start int) {
	if !r.bad && r.i == start {
		r.skipValue()
	}
}

// open consumes c, the opening bracket, when it is next; past maxDepth it
// sets Err instead.
func (r *Reader) open(c byte) bool {
	if r.bad {
		return false
	}
	r.skipSpace()
	if r.i >= len(r.b) || r.b[r.i] != c {
		return false
	}
	if r.depth >= maxDepth {
		r.bad = true
		return false
	}
	r.depth++
	r.i++
	return true
}

// close consumes c and ends the container when it is next.
func (r *Reader) close(c byte) bool {
	r.skipSpace()
	if r.bad || r.i >= len(r.b) || r.b[r.i] != c {
		return false
	}
	r.i++
	r.depth--
	return true
}

func (r *Reader) comma() bool {
	if r.i >= len(r.b) || r.b[r.i] != ',' {
		return false
	}
	r.i++
	return true
}

func (r *Reader) fail() bool {
	r.bad = true
	return true
}

func (r *Reader) skipSpace() {
	for r.i < len(r.b) && isSpace(r.b[r.i]) {
		r.i++
	}
}

func (r *Reader) key() ([]byte, bool) {
	r.skipSpace()
	if r.i >= len(r.b) || r.b[r.i] != '"' {
		return nil, false
	}
	start := r.i
	if escaped := r.skipString(); r.bad || escaped {
		return nil, false
	}
	return r.b[start+1 : r.i-1], true
}

func (r *Reader) skipValue() {
	if r.i >= len(r.b) {
		r.bad = true
		return
	}
	switch c := r.b[r.i]; {
	case c == '"':
		r.skipString()
	case c == '{':
		r.Object(func([]byte) {})
	case c == '[':
		r.Array(func(int) {})
	case c == 't':
		r.literal("true")
	case c == 'f':
		r.literal("false")
	case c == 'n':
		r.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		r.skipNumber()
	default:
		r.bad = true
	}
}

func (r *Reader) literal(s string) {
	if len(r.b)-r.i < len(s) || string(r.b[r.i:r.i+len(s)]) != s {
		r.bad = true
		return
	}
	r.i += len(s)
}

// skipNumber consumes a number by the JSON grammar; what follows it is
// checked by the caller as a separator.
func (r *Reader) skipNumber() {
	b, i := r.b, r.i
	if b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case i < len(b) && b[i] >= '1' && b[i] <= '9':
		i = skipDigits(b, i)
	default:
		r.bad = true
		return
	}
	if i < len(b) && b[i] == '.' {
		if i++; i >= len(b) || !isDigit(b[i]) {
			r.bad = true
			return
		}
		i = skipDigits(b, i)
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		if i++; i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= len(b) || !isDigit(b[i]) {
			r.bad = true
			return
		}
		i = skipDigits(b, i)
	}
	r.i = i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func skipDigits(b []byte, i int) int {
	for i < len(b) && isDigit(b[i]) {
		i++
	}
	return i
}

// skipString consumes the string at the cursor and reports whether it holds
// an escape. Like encoding/json it rejects control characters and malformed
// escapes but not invalid UTF-8.
func (r *Reader) skipString() (escaped bool) {
	b, i := r.b, r.i+1
	for {
		if i = nextStringSpecial(b, i); i >= len(b) {
			r.bad = true
			return escaped
		}
		switch c := b[i]; {
		case c == '"':
			r.i = i + 1
			return escaped
		case c == '\\':
			escaped = true
			if i+1 >= len(b) {
				r.bad = true
				return escaped
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+6 > len(b) || !isHex(b[i+2]) || !isHex(b[i+3]) || !isHex(b[i+4]) || !isHex(b[i+5]) {
					r.bad = true
					return escaped
				}
				i += 6
			default:
				r.bad = true
				return escaped
			}
		default: // a control character
			r.bad = true
			return escaped
		}
	}
}

// nextStringSpecial returns the index of the first quote, backslash or
// control character in b at or after i, or len(b), eight bytes at a time.
func nextStringSpecial(b []byte, i int) int {
	for ; i+8 <= len(b); i += 8 {
		if m := stringSpecials(binary.LittleEndian.Uint64(b[i:])); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
	}
	for ; i < len(b); i++ {
		if c := b[i]; c == '"' || c == '\\' || c < 0x20 {
			return i
		}
	}
	return i
}

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

const (
	lowBits  = 0x0101010101010101
	highBits = 0x8080808080808080
)

// stringSpecials sets the high bit of each of the eight bytes in w that is a
// quote, a backslash or a control character. Borrows may also set it on bytes
// after the first such byte, never before it, so the lowest set bit is exact.
func stringSpecials(w uint64) uint64 {
	quote := w ^ (lowBits * '"')
	slash := w ^ (lowBits * '\\')
	return ((quote-lowBits)&^quote | (slash-lowBits)&^slash | (w-lowBits*0x20)&^w) & highBits
}

// String decodes v, a JSON string, the way encoding/json decodes it into a Go
// string; false when v is not a string.
func String(v []byte) (string, bool) {
	if raw, ok := verbatim(v); ok {
		return string(raw), true
	}
	if len(v) < 2 || v[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// StringDecodesTo reports whether v, a JSON string, decodes to s, without
// decoding unless v holds an escape or invalid UTF-8.
func StringDecodesTo(v []byte, s string) bool {
	if raw, ok := verbatim(v); ok {
		return string(raw) == s
	}
	d, ok := String(v)
	return ok && d == s
}

// verbatim returns the contents of v, a JSON string, when they decode to
// themselves: no escape and valid UTF-8.
func verbatim(v []byte) ([]byte, bool) {
	if len(v) < 2 || v[0] != '"' {
		return nil, false
	}
	s := v[1 : len(v)-1]
	return s, bytes.IndexByte(s, '\\') < 0 && utf8.Valid(s)
}
