package inference

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/bits"
	"net/http"
	"strings"
)

// bodyError is a request body relay refuses, carrying the API error it is answered with.
type bodyError struct {
	status int
	code   string
	msg    string
}

func (e *bodyError) Error() string { return e.msg }

var (
	errDuplicateModel   = &bodyError{http.StatusBadRequest, "duplicate_model", "request body has more than one top-level 'model' field"}
	errMissingModel     = &bodyError{http.StatusBadRequest, "missing_model", "field 'model' is required"}
	errModelNotString   = &bodyError{http.StatusBadRequest, "parse_error", "invalid JSON: field 'model' must be a string"}
	errModelKeySpelling = &bodyError{http.StatusBadRequest, "invalid_model_key", "proxy mode forwards the body as sent: its top-level 'model' key must be spelled exactly \"model\", without escapes or other letter case"}
)

func jsonSyntaxError(format string, args ...any) *bodyError {
	return &bodyError{http.StatusBadRequest, "parse_error", "invalid JSON: " + fmt.Sprintf(format, args...)}
}

// maxScanDepth matches encoding/json's nesting limit, so no body it accepts is refused here.
const maxScanDepth = 10000

// maxModelKeyBytes bounds a captured top-level key: "model" spelled entirely in \u escapes is 30 bytes, so a longer key cannot be it.
const maxModelKeyBytes = 32

const (
	scanValue    uint8 = iota // a value
	scanObjFirst              // after '{': a key or '}'
	scanObjKey                // after ',' in an object: a key
	scanColon                 // after a key: ':'
	scanArrFirst              // after '[': a value or ']'
	scanAfter                 // after a value: ',' or the container's close
	scanString                // inside a string
	scanScalar                // inside a number or literal
	scanDone                  // top-level object closed: whitespace only
)

// What the string being read is.
const (
	strPlain    uint8 = iota
	strTopKey         // a key of the top-level object
	strModel          // the model value
	strInnerKey       // a key of a nested object
)

// Number grammar states; a literal is tracked by the bytes still expected.
const (
	numSign  uint8 = iota // after '-'
	numZero               // a leading 0
	numInt                // integer digits
	numDot                // after '.'
	numFrac               // fraction digits
	numE                  // after 'e'
	numESign              // after the exponent sign
	numExp                // exponent digits
)

// modelScanner finds the top-level "model" member of a JSON object request body without decoding the body: one byte-level pass that tracks nesting, string and escape state and the scalar grammar, decoding only top-level keys and the model value. It is fed the body in order, in chunks of any size.
//
// A second top-level model key is rejected: relay routes, meters and prices on one value while the upstream may honour the other. Keys match "model" case-insensitively, as encoding/json does. Strings are not checked for control characters or escape validity outside the keys and model value it decodes; an upstream rejects those bodies itself.
type modelScanner struct {
	// exactKey refuses a model key spelled other than the bytes `model`, for bodies forwarded as sent: a case-sensitive upstream would not see a model there and could serve its default.
	exactKey bool

	state    uint8
	strKind  uint8
	numState uint8
	literal  string // rest of the literal being read; empty while reading a number
	bsOdd    bool   // the string read so far ends in an odd run of backslashes
	isNumber bool

	depth  int
	arrays uint64   // bit i: container at depth i+1 is an array (first 64 levels)
	deeper []uint64 // the same past 64 levels
	off    int64    // bytes fed before the current chunk

	keyRaw    []byte
	keyLong   bool
	modelNext bool // the value being read belongs to the model key
	modelKeys int
	modelRaw  []byte
	model     string
	modelDone bool // the model value has been read; model is "" for null
	err       *bodyError
}

// feed scans the next chunk. It returns the index in p of the byte that closed the top-level object, or -1 when the object did not close in p. After an error, feed is a no-op and err holds it.
func (s *modelScanner) feed(p []byte) (closeAt int) {
	closeAt = -1
	if s.err != nil {
		return closeAt
	}
	defer func() { s.off += int64(len(p)) }()
	for i := 0; i < len(p); {
		switch s.state {
		case scanString, scanScalar:
			if s.state == scanString {
				i = s.readString(p, i)
			} else {
				i = s.readScalar(p, i)
			}
			if s.err != nil {
				return -1
			}
			continue
		}
		c := p[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		switch s.state {
		case scanValue:
			s.beginValue(c, i)
		case scanArrFirst:
			if c == ']' {
				s.close()
			} else {
				s.beginValue(c, i)
			}
		case scanObjFirst:
			switch c {
			case '}':
				s.close()
			case '"':
				s.beginKey()
			default:
				s.fail(c, i, "looking for beginning of object key string")
			}
		case scanObjKey:
			if c == '"' {
				s.beginKey()
			} else {
				s.fail(c, i, "looking for beginning of object key string")
			}
		case scanColon:
			if c == ':' {
				s.state = scanValue
			} else {
				s.fail(c, i, "after object key")
			}
		case scanAfter:
			inArray := s.inArray()
			switch {
			case c == ',' && inArray:
				s.state = scanValue
			case c == ',':
				s.state = scanObjKey
			case c == ']' && inArray, c == '}' && !inArray:
				s.close()
			default:
				s.fail(c, i, "after value")
			}
		case scanDone:
			s.fail(c, i, "after top-level value")
		}
		if s.err != nil {
			return -1
		}
		if s.state == scanDone && closeAt < 0 {
			closeAt = i
		}
		i++
	}
	return closeAt
}

// finish reports whether the body fed so far is a complete object with exactly one non-empty string model.
func (s *modelScanner) finish() *bodyError {
	if s.err != nil {
		return s.err
	}
	if s.state != scanDone {
		return jsonSyntaxError("unexpected end of JSON input")
	}
	if s.model == "" {
		return errMissingModel
	}
	return nil
}

func (s *modelScanner) fail(c byte, i int, context string) {
	s.err = jsonSyntaxError("invalid character %q %s at offset %d", c, context, s.off+int64(i))
}

func (s *modelScanner) beginValue(c byte, i int) {
	if s.depth == 0 {
		if c != '{' {
			s.fail(c, i, "looking for beginning of an object")
			return
		}
		s.push(false)
		s.state = scanObjFirst
		return
	}
	isModel := s.modelNext
	s.modelNext = false
	s.strKind = strPlain
	if isModel {
		s.strKind = strModel
	}
	scalar := c == '-' || c == 't' || c == 'f' || c == 'n' || c >= '0' && c <= '9'
	// null is the one non-string model value encoding/json takes, as an absent model.
	if isModel && c != '"' && c != 'n' && (scalar || c == '{' || c == '[') {
		s.err = errModelNotString
		return
	}
	switch {
	case c == '"':
		s.state = scanString
	case c == '{':
		s.push(false)
		s.state = scanObjFirst
	case c == '[':
		s.push(true)
		s.state = scanArrFirst
	case scalar:
		s.beginScalar(c)
	default:
		s.fail(c, i, "looking for beginning of value")
	}
}

func (s *modelScanner) beginKey() {
	s.state = scanString
	s.strKind = strInnerKey
	if s.depth == 1 {
		s.strKind = strTopKey
		s.keyRaw = s.keyRaw[:0]
		s.keyLong = false
	}
}

func (s *modelScanner) push(array bool) {
	if s.depth == maxScanDepth {
		s.err = jsonSyntaxError("exceeded max depth")
		return
	}
	if d := s.depth; d < 64 {
		if array {
			s.arrays |= 1 << d
		} else {
			s.arrays &^= 1 << d
		}
	} else {
		w, b := (d-64)/64, uint((d-64)%64)
		for len(s.deeper) <= w {
			s.deeper = append(s.deeper, 0)
		}
		if array {
			s.deeper[w] |= 1 << b
		} else {
			s.deeper[w] &^= 1 << b
		}
	}
	s.depth++
}

func (s *modelScanner) inArray() bool {
	d := s.depth - 1
	if d < 64 {
		return s.arrays&(1<<d) != 0
	}
	return s.deeper[(d-64)/64]&(1<<uint((d-64)%64)) != 0
}

func (s *modelScanner) close() {
	s.depth--
	if s.depth == 0 {
		s.state = scanDone
		return
	}
	s.state = scanAfter
}

// readString consumes string content from p[i:] and returns the index after it: past the closing quote, or len(p) when the string continues into the next chunk. A quote is the string's end unless an odd run of backslashes precedes it.
func (s *modelScanner) readString(p []byte, i int) int {
	seg := i
	for {
		q := indexQuote(p, i)
		if q < 0 {
			s.bsOdd = endsInOddBackslashes(p[seg:], s.bsOdd)
			s.capture(p[seg:])
			return len(p)
		}
		if (q > seg && p[q-1] != '\\') || !endsInOddBackslashes(p[seg:q], s.bsOdd) {
			s.capture(p[seg:q])
			s.bsOdd = false
			s.endString()
			return q + 1
		}
		i = q + 1
	}
}

const (
	swarOnes  = 0x0101010101010101
	swarHigh  = 0x8080808080808080
	swarQuote = '"' * swarOnes
)

// indexQuote returns the index of the first '"' in p at or after i, or -1. It tests eight bytes per step: escaped text has a quote every few bytes, where bytes.IndexByte's per-call setup costs more than the search.
func indexQuote(p []byte, i int) int {
	b := p[i:]
	for len(b) >= 8 {
		x := binary.LittleEndian.Uint64(b) ^ swarQuote
		// Exact for the lowest zero byte, which is all this needs.
		if m := (x - swarOnes) &^ x & swarHigh; m != 0 {
			return len(p) - len(b) + bits.TrailingZeros64(m)>>3
		}
		b = b[8:]
	}
	for j, c := range b {
		if c == '"' {
			return len(p) - len(b) + j
		}
	}
	return -1
}

// endsInOddBackslashes reports whether b ends in an odd run of backslashes; carry is the parity of the run b continues.
func endsInOddBackslashes(b []byte, carry bool) bool {
	n := 0
	for n < len(b) && b[len(b)-1-n] == '\\' {
		n++
	}
	odd := n%2 == 1
	if n == len(b) && carry {
		odd = !odd
	}
	return odd
}

func (s *modelScanner) capture(b []byte) {
	switch s.strKind {
	case strTopKey:
		if s.keyLong || len(s.keyRaw)+len(b) > maxModelKeyBytes {
			s.keyLong = true
			return
		}
		s.keyRaw = append(s.keyRaw, b...)
	case strModel:
		s.modelRaw = append(s.modelRaw, b...)
	}
}

func (s *modelScanner) endString() {
	switch s.strKind {
	case strTopKey, strInnerKey:
		s.state = scanColon
		if s.strKind == strTopKey && !s.keyLong {
			isModel, err := isModelKey(s.keyRaw)
			if err != nil {
				s.err = err
				return
			}
			if isModel {
				s.modelKeys++
				if s.modelKeys > 1 {
					s.err = errDuplicateModel
					return
				}
				if s.exactKey && string(s.keyRaw) != "model" {
					s.err = errModelKeySpelling
					return
				}
				s.modelNext = true
			}
		}
		return
	case strModel:
		m, err := decodeJSONString(s.modelRaw)
		if err != nil {
			s.err = err
			return
		}
		s.model, s.modelDone, s.modelRaw = m, true, nil
	}
	s.state = scanAfter
}

var modelKeyName = []byte("model")

func isModelKey(raw []byte) (bool, *bodyError) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return bytes.EqualFold(raw, modelKeyName), nil
	}
	k, err := decodeJSONString(raw)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(k, "model"), nil
}

// decodeJSONString decodes the raw content of a JSON string (no quotes) as encoding/json would.
func decodeJSONString(raw []byte) (string, *bodyError) {
	plain := true
	for _, c := range raw {
		if c < 0x20 || c >= 0x7f || c == '\\' {
			plain = false
			break
		}
	}
	if plain {
		return string(raw), nil
	}
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(append(append(quoted, '"'), raw...), '"')
	var out string
	if err := json.Unmarshal(quoted, &out); err != nil {
		return "", jsonSyntaxError("%v", err)
	}
	return out, nil
}

func (s *modelScanner) beginScalar(c byte) {
	s.state = scanScalar
	s.isNumber = c == '-' || c >= '0' && c <= '9'
	switch {
	case c == '-':
		s.numState = numSign
	case c == '0':
		s.numState = numZero
	case s.isNumber:
		s.numState = numInt
	case c == 't':
		s.literal = "rue"
	case c == 'f':
		s.literal = "alse"
	default:
		s.literal = "ull"
	}
}

// readScalar consumes a number or literal from p[i:] and returns the index after it; at the first byte that cannot continue it, the scalar must be complete.
func (s *modelScanner) readScalar(p []byte, i int) int {
	for ; i < len(p); i++ {
		c := p[i]
		if s.isNumber {
			if !s.stepNumber(c) {
				break
			}
			continue
		}
		if s.literal == "" || c != s.literal[0] {
			break
		}
		s.literal = s.literal[1:]
	}
	if i == len(p) {
		return i
	}
	complete := s.literal == ""
	if s.isNumber {
		complete = s.numState == numZero || s.numState == numInt || s.numState == numFrac || s.numState == numExp
	}
	if !complete {
		s.fail(p[i], i, "in literal or number")
		return i
	}
	if s.strKind == strModel {
		s.model, s.modelDone = "", true
	}
	s.strKind = strPlain
	s.state = scanAfter
	return i
}

// stepNumber advances the number grammar by c, reporting whether c continues the number.
func (s *modelScanner) stepNumber(c byte) bool {
	digit := c >= '0' && c <= '9'
	switch s.numState {
	case numSign:
		switch {
		case c == '0':
			s.numState = numZero
		case digit:
			s.numState = numInt
		default:
			return false
		}
	case numZero, numInt:
		switch {
		case digit && s.numState == numInt:
		case c == '.':
			s.numState = numDot
		case c == 'e' || c == 'E':
			s.numState = numE
		default:
			return false
		}
	case numDot:
		if !digit {
			return false
		}
		s.numState = numFrac
	case numFrac:
		switch {
		case digit:
		case c == 'e' || c == 'E':
			s.numState = numE
		default:
			return false
		}
	case numE:
		switch {
		case c == '+' || c == '-':
			s.numState = numESign
		case digit:
			s.numState = numExp
		default:
			return false
		}
	case numESign:
		if !digit {
			return false
		}
		s.numState = numExp
	case numExp:
		return digit
	}
	return true
}
