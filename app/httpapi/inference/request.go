package inference

import (
	"bytes"
	"encoding/json"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ParsedRequest is a request body read in one pass: the values relay acts on, and the byte offsets of every top-level member, so a body sent upstream with edits is assembled from the original bytes instead of being decoded and re-encoded. It holds no copy of the body; typical bodies fit the inline member array, about 1 KB.
type ParsedRequest struct {
	// Model is the top-level model, matched case-insensitively as encoding/json does; "" when absent or null.
	Model string

	// Stream reports whether the request selects streaming through the member its shape's RequestKeys name.
	Stream bool

	obj     jsonObject
	modelAt int // index of the model member, -1 when there is none
	inline  [16]jsonMember
}

func fieldTypeError(key, kind string) *bodyError {
	return jsonSyntaxError("field '%s' must be %s", key, kind)
}

// parseRequest reads body once: it refuses what encoding/json refuses, then a model or stream member of the wrong type (parse_error), then a second top-level model key (duplicate_model), in that order, as the probe decode it replaces did. A missing model is the caller's to refuse; a top-level null reads as an empty request.
func parseRequest(body []byte, keys v1.RequestKeys) (*ParsedRequest, *bodyError) {
	p := &ParsedRequest{modelAt: -1}
	obj, err := scanJSONObject(body, p.inline[:])
	if err != nil {
		return nil, err
	}
	p.obj = obj
	if !obj.isObject {
		if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
			return p, nil
		}
		return nil, jsonSyntaxError("request body must be a JSON object")
	}
	var typeErr *bodyError
	modelKeys := 0
	var streamBool, streamSet bool
	var streamStr string
	for i := range obj.members {
		m := &obj.members[i]
		v := body[m.valStart:m.valEnd]
		if obj.keyFold(i, "model") {
			modelKeys++
			p.modelAt = i
			switch v[0] {
			case '"':
				if p.Model, err = decodeJSONString(v[1 : len(v)-1]); err != nil {
					return nil, err
				}
			case 'n': // null leaves a string as it was
			default:
				if typeErr == nil {
					typeErr = errModelNotString
				}
			}
		}
		if keys.Stream == "" || !obj.keyFold(i, keys.Stream) {
			continue
		}
		if keys.StreamOn == "" {
			switch v[0] {
			case 't', 'f':
				streamBool, streamSet = v[0] == 't', true
			case 'n': // null resets a *bool
				streamSet = false
			default:
				if typeErr == nil {
					typeErr = fieldTypeError(keys.Stream, "a boolean")
				}
			}
			continue
		}
		switch v[0] {
		case '"':
			if streamStr, err = decodeJSONString(v[1 : len(v)-1]); err != nil {
				return nil, err
			}
		case 'n':
		default:
			if typeErr == nil {
				typeErr = fieldTypeError(keys.Stream, "a string")
			}
		}
	}
	if typeErr != nil {
		return nil, typeErr
	}
	if modelKeys > 1 {
		return nil, errDuplicateModel
	}
	if keys.StreamOn == "" {
		p.Stream = streamSet && streamBool
	} else {
		p.Stream = streamStr == keys.StreamOn
	}
	return p, nil
}

// Body returns the body the request was parsed from.
func (p *ParsedRequest) Body() []byte { return p.obj.b }

// Member returns the raw value of the last top-level member whose key is exactly key (escapes decoded), nil when there is none: what a decode into map[string]json.RawMessage would hold for key.
func (p *ParsedRequest) Member(key string) []byte {
	if i := p.obj.lastIndex(key); i >= 0 {
		m := p.obj.members[i]
		return p.obj.b[m.valStart:m.valEnd]
	}
	return nil
}

// memberEdit sets a top-level member to value, or removes it when value is nil.
type memberEdit struct {
	key   string
	value []byte
}

// upstreamBody returns the body with its model set to upstreamModel and edits applied; the original slice when that changes nothing.
func (p *ParsedRequest) upstreamBody(upstreamModel string, edits []memberEdit) []byte {
	if !p.obj.isObject {
		return p.obj.b
	}
	if len(edits) == 0 && p.modelAt >= 0 && p.obj.keyIs(p.modelAt, "model") && p.Model == upstreamModel {
		return p.obj.b
	}
	return p.obj.assemble(append(edits, memberEdit{key: "model", value: appendJSONString(nil, upstreamModel)}))
}

// stripParams appends to edits the removal of each unsupported param present in the body at its path in paths, and returns the params removed. A dotted path addresses a member of nested objects.
func (p *ParsedRequest) stripParams(edits []memberEdit, unsupported []string, paths map[string]string) ([]memberEdit, []string) {
	if !p.obj.isObject || len(paths) == 0 {
		return edits, nil
	}
	var dropped []string
	for _, param := range unsupported {
		path, ok := paths[param]
		if !ok {
			continue
		}
		top, rest, nested := strings.Cut(path, ".")
		at := editIndex(edits, top)
		if at >= 0 && edits[at].value == nil {
			continue
		}
		if !nested {
			switch {
			case at >= 0:
				edits[at].value = nil
			case p.obj.lastIndex(top) >= 0:
				edits = append(edits, memberEdit{key: top})
			default:
				continue
			}
			dropped = append(dropped, param)
			continue
		}
		current := p.Member(top)
		if at >= 0 {
			current = edits[at].value
		}
		value, ok := deleteNested(current, strings.Split(rest, "."))
		if !ok {
			continue
		}
		if at >= 0 {
			edits[at].value = value
		} else {
			edits = append(edits, memberEdit{key: top, value: value})
		}
		dropped = append(dropped, param)
	}
	return edits, dropped
}

func editIndex(edits []memberEdit, key string) int {
	for i, e := range edits {
		if e.key == key {
			return i
		}
	}
	return -1
}

// deleteNested removes the member addressed by segs from the object raw, reporting false when raw is not an object or has no such member.
func deleteNested(raw []byte, segs []string) ([]byte, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var inline [16]jsonMember
	obj, err := scanJSONObject(raw, inline[:])
	if err != nil {
		return nil, false
	}
	at := obj.lastIndex(segs[0])
	if at < 0 {
		return nil, false
	}
	if len(segs) == 1 {
		return obj.assemble([]memberEdit{{key: segs[0]}}), true
	}
	m := obj.members[at]
	value, ok := deleteNested(raw[m.valStart:m.valEnd], segs[1:])
	if !ok {
		return nil, false
	}
	return obj.assemble([]memberEdit{{key: segs[0], value: value}}), true
}

// appendJSONString appends s as a JSON string literal, escaping as encoding/json does without HTML escaping.
func appendJSONString(dst []byte, s string) []byte {
	plain := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			plain = false
			break
		}
	}
	if plain {
		return append(append(append(dst, '"'), s...), '"')
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}
