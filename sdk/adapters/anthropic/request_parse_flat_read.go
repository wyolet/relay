package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
)

// readFields fills vals[i] with the raw value of the member of obj named
// names[i], nil when absent. It reports false when obj is not an object, a key
// holds an escape, or two members match one name: there the layered probes
// may each see a different value. obj must come from a body json.Valid
// accepted, since jsonscan does not validate.
func readFields(obj []byte, names []string, vals [][]byte) bool {
	if len(obj) == 0 || obj[0] != '{' {
		return false
	}
	m := jsonscan.Object(obj)
	for m.Next() {
		for i, name := range names {
			if jsonscan.KeyIs(m.Key(), name) {
				if vals[i] != nil {
					return false
				}
				vals[i] = m.Value()
				break
			}
		}
	}
	return !m.Err()
}

// stringField decodes a raw member value bound for a string field: absent and
// null leave it empty, any other non-string would fail the decode.
func stringField(v []byte) (string, bool) {
	switch {
	case v == nil || jsonscan.IsNull(v):
		return "", true
	case v[0] == '"':
		return unquote(v), true
	}
	return "", false
}

// structField decodes a raw member value bound for a pointer field; absent and
// null leave it nil.
func structField[T any](v []byte, dst **T) bool {
	if v == nil || jsonscan.IsNull(v) {
		return true
	}
	return json.Unmarshal(v, dst) == nil
}

// unquote returns the string a valid JSON string literal decodes to. A literal
// holding a \u escape or invalid UTF-8 goes through encoding/json, which owns
// surrogate pairs and U+FFFD replacement.
func unquote(lit []byte) string {
	s := lit[1 : len(lit)-1]
	i := bytes.IndexByte(s, '\\')
	if !utf8.Valid(s) || (i >= 0 && bytes.Contains(s[i:], []byte(`\u`))) {
		var out string
		_ = json.Unmarshal(lit, &out)
		return out
	}
	if i < 0 {
		return string(s)
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i >= 0 {
		sb.Write(s[:i])
		switch c := s[i+1]; c {
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		default: // '"', '\\', '/'
			sb.WriteByte(c)
		}
		s = s[i+2:]
		i = bytes.IndexByte(s, '\\')
	}
	sb.Write(s)
	return sb.String()
}

// contentBlock holds every field any layered probe reads from a content block.
type contentBlock struct {
	typ, text, id, name, thinking, signature, toolUseID string
	input, content                                      []byte // raw, nil when absent
	source                                              *blockSource
	cacheControl                                        *wireCacheControl
}

var contentBlockFields = [...]string{"type", "text", "id", "name", "thinking", "signature", "tool_use_id", "input", "content", "source", "cache_control"}

func readContentBlock(raw []byte) (contentBlock, bool) {
	var v [len(contentBlockFields)][]byte
	var b contentBlock
	if !readFields(raw, contentBlockFields[:], v[:]) {
		return b, false
	}
	for i, dst := range [...]*string{&b.typ, &b.text, &b.id, &b.name, &b.thinking, &b.signature, &b.toolUseID} {
		s, ok := stringField(v[i])
		if !ok {
			return b, false
		}
		*dst = s
	}
	b.input, b.content = v[7], v[8]
	return b, structField(v[9], &b.source) && structField(v[10], &b.cacheControl)
}

func readContentBlocks(raw []byte) ([]contentBlock, bool) {
	var blocks []contentBlock
	els := jsonscan.Array(raw)
	for els.Next() {
		b, ok := readContentBlock(els.Value())
		if !ok {
			return nil, false
		}
		blocks = append(blocks, b)
	}
	return blocks, !els.Err()
}
