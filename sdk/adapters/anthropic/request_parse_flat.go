package anthropic

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// parseRequestFlat decodes a request in one walk: json.Valid checks the body
// once, system, tools and messages are read member by member from the raw
// bytes, and each string is unquoted once from its literal. It returns the
// same request parseRequestLayered would, and declines (ok false) every body
// where that is not guaranteed: invalid JSON, a key holding an escape, a field
// repeated within one object, a mistyped field, or a value the layered decode
// rejects.
func parseRequestFlat(body []byte) (*v1.Request, bool) {
	if !json.Valid(body) {
		return nil, false
	}
	var system, tools, messages []byte
	// The other members go through encoding/json as one small object, so the
	// scalar fields keep its exact semantics.
	scalars := []byte{'{'}
	top := jsonscan.Object(body)
	for top.Next() {
		var slot *[]byte
		switch key := top.Key(); {
		case jsonscan.KeyIs(key, "system"):
			slot = &system
		case jsonscan.KeyIs(key, "tools"):
			slot = &tools
		case jsonscan.KeyIs(key, "messages"):
			slot = &messages
		}
		if slot == nil {
			if len(scalars) > 1 {
				scalars = append(scalars, ',')
			}
			scalars = append(append(append(scalars, '"'), top.Key()...), `":`...)
			scalars = append(scalars, top.Value()...)
			continue
		}
		if *slot != nil {
			return nil, false
		}
		*slot = top.Value()
	}
	if top.Err() {
		return nil, false
	}
	var wire requestScalars
	if json.Unmarshal(append(scalars, '}'), &wire) != nil || wire.Model == "" {
		return nil, false
	}
	if !isArrayOrNull(tools) || !isArrayOrNull(messages) {
		return nil, false
	}
	req := wire.toCanonical()

	var breakpoints cacheBreakpoints
	var instructionsCached, toolsCached, ok bool
	if system != nil && !jsonscan.IsNull(system) {
		if req.Instructions, instructionsCached, ok = systemToCanonical(system, &breakpoints); !ok {
			return nil, false
		}
	}
	if tools != nil && !jsonscan.IsNull(tools) {
		if req.Tools, toolsCached, ok = toolsToCanonical(tools, wire.ToolChoice, &breakpoints); !ok {
			return nil, false
		}
	}
	if messages != nil && !jsonscan.IsNull(messages) {
		if req.Input, ok = messagesToCanonical(messages, &breakpoints); !ok {
			return nil, false
		}
	}
	req.CacheConfig = breakpoints.config(instructionsCached, toolsCached)
	return req, true
}

func isArrayOrNull(v []byte) bool {
	return v == nil || v[0] == '[' || jsonscan.IsNull(v)
}

// systemToCanonical mirrors anthropicExtractSystemText and systemMarked.
func systemToCanonical(raw []byte, breakpoints *cacheBreakpoints) (string, bool, bool) {
	switch raw[0] {
	case '"':
		return unquote(raw), false, true
	case '[':
	default:
		return "", false, true
	}
	blocks, ok := readContentBlocks(raw)
	if !ok {
		return "", false, false
	}
	var text []byte
	marked := false
	for i := range blocks {
		b := &blocks[i]
		if b.typ == "text" && b.text != "" {
			if len(text) > 0 {
				text = append(text, '\n')
			}
			text = append(text, b.text...)
		}
		if breakpoints.note(cacheControlProbe{CacheControl: b.cacheControl}) {
			marked = true
		}
	}
	return string(text), marked, true
}

var toolFields = [...]string{"name", "type", "description", "input_schema", "cache_control"}

// toolsToCanonical mirrors the layered tools loop: anthropicParseTool and
// blockMarked per tool, then the tool choice.
func toolsToCanonical(raw []byte, toolChoice json.RawMessage, breakpoints *cacheBreakpoints) (*v1.ToolsConfig, bool, bool) {
	tc := &v1.ToolsConfig{}
	cached, seen := false, false
	els := jsonscan.Array(raw)
	for els.Next() {
		seen = true
		var v [len(toolFields)][]byte
		if !readFields(els.Value(), toolFields[:], v[:]) {
			return nil, false, false
		}
		name, ok1 := stringField(v[0])
		typ, ok2 := stringField(v[1])
		description, ok3 := stringField(v[2])
		var cc *wireCacheControl
		if !ok1 || !ok2 || !ok3 || !structField(v[4], &cc) {
			return nil, false, false
		}
		if typ != "" && typ != "function" {
			tc.Definitions = append(tc.Definitions, &v1.ServerTool{Name: name})
		} else {
			schema := json.RawMessage(`{}`)
			if v[3] != nil {
				schema = append(json.RawMessage(nil), v[3]...)
			}
			tc.Definitions = append(tc.Definitions, &v1.FunctionTool{Name: name, Description: description, Parameters: schema})
		}
		if breakpoints.note(cacheControlProbe{CacheControl: cc}) {
			cached = true
		}
	}
	if els.Err() {
		return nil, false, false
	}
	if !seen {
		return nil, false, true
	}
	if len(toolChoice) > 0 && string(toolChoice) != "null" {
		tc.Choice, tc.Parallel = anthropicParseToolChoice(toolChoice)
	}
	return tc, cached, true
}

var messageFields = [...]string{"role", "content"}

// messagesToCanonical mirrors anthropicMessagesToCanonical.
func messagesToCanonical(raw []byte, breakpoints *cacheBreakpoints) ([]v1.Item, bool) {
	var items []v1.Item
	els := jsonscan.Array(raw)
	for els.Next() {
		var v [len(messageFields)][]byte
		if !readFields(els.Value(), messageFields[:], v[:]) {
			return nil, false
		}
		role, ok := stringField(v[0])
		if !ok {
			return nil, false
		}
		if items, ok = appendMessageItems(items, role, v[1], breakpoints); !ok {
			return nil, false
		}
	}
	return items, !els.Err()
}
