package adapters_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// flattenRequest reduces a canonical request to leaf path → JSON value.
// Arrays of objects are keyed by identity rather than position (input items by
// their text or call id, tools by name, parts by type), so one dropped element
// shows up as that element's paths instead of shifting every later index.
// Scalar arrays stay one leaf.
func flattenRequest(v any) (map[string]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenInto(out, "", doc)
	return out, nil
}

// opaqueKeys hold caller JSON canonical never looks inside; each is one leaf.
var opaqueKeys = map[string]bool{"parameters": true, "schema": true, "provider_data": true}

func flattenInto(out map[string]string, prefix string, v any) {
	last := prefix[strings.LastIndex(prefix, ".")+1:]
	if opaqueKeys[last] || strings.HasPrefix(prefix, "extensions.") {
		b, _ := json.Marshal(v)
		out[prefix] = string(b)
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, child := range x {
			key := k
			if prefix == "model_config" {
				key = "*" // one model per request; the key is the model name
			}
			flattenInto(out, joinPath(prefix, key), child)
		}
	case []any:
		if !allObjects(x) {
			b, _ := json.Marshal(x)
			out[prefix] = string(b)
			return
		}
		seen := map[string]int{}
		for _, el := range x {
			obj := el.(map[string]any)
			key := elementKey(obj)
			seen[key]++
			if n := seen[key]; n > 1 {
				key = fmt.Sprintf("%s#%d", key, n)
			}
			flattenInto(out, prefix+"["+key+"]", obj)
		}
	default:
		b, _ := json.Marshal(x)
		out[prefix] = string(b)
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func allObjects(xs []any) bool {
	if len(xs) == 0 {
		return false
	}
	for _, x := range xs {
		if _, ok := x.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// elementKey names an array element by what identifies it across a round
// trip: a tool by name, a message by its first text, a call by name and
// arguments (call ids are re-minted by shapes that have none), a result by
// call id, a part or annotation by type.
func elementKey(obj map[string]any) string {
	typ, _ := obj["type"].(string)
	switch typ {
	case "message":
		return "message:" + firstText(obj["content"])
	case "function_call":
		return "function_call:" + str(obj["name"]) + str(obj["arguments"])
	case "function_call_output":
		return "function_call_output:" + str(obj["call_id"])
	case "reasoning":
		return "reasoning"
	case "function", "server", "mcp":
		return typ + ":" + str(obj["name"])
	}
	if typ != "" {
		return typ
	}
	if t, ok := obj["text"].(string); ok {
		return "text:" + t
	}
	return "?"
}

func firstText(content any) string {
	parts, _ := content.([]any)
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// lostPaths returns the leaves of before that are missing from after or carry
// a different value there, sorted.
func lostPaths(before, after map[string]string) []string {
	var lost []string
	for p, v := range before {
		if got, ok := after[p]; !ok || got != v {
			lost = append(lost, p)
		}
	}
	sort.Strings(lost)
	return lost
}

// globMatch reports whether path matches pattern, where * matches any run of
// characters (dots and brackets included) and everything else is literal.
func globMatch(pattern, path string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(path)
}
