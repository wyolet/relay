package inference

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// rewriteModelField surgically replaces the top-level "model" field of a
// JSON request body with newName. Nested values (messages, tools, ...)
// keep their original bytes thanks to json.RawMessage round-tripping.
//
// Top-level key order is not preserved (Go map iteration), which is fine
// for every upstream we target. Returns the original body unchanged if the
// new name equals the existing value or if the body isn't a JSON object.
func rewriteModelField(body []byte, newName string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	cur, ok := fields["model"]
	if ok {
		var s string
		if err := json.Unmarshal(cur, &s); err == nil && s == newName {
			return body
		}
	}
	fields["model"] = json.RawMessage(strconv.Quote(newName))

	var buf bytes.Buffer
	buf.Grow(len(body) + len(newName))
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return body
	}
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}
