package openai

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/sse"
)

// RequestStreamUsage sets stream_options.include_usage on a streamed Chat
// Completions request, given its top-level fields, because without it the
// stream never reports token usage. Reports whether it changed the fields:
// false when the request is not streamed, already asks for usage, or carries
// a stream_options value that is not an object (left for the upstream to
// reject).
func RequestStreamUsage(fields map[string]json.RawMessage) bool {
	var stream bool
	if raw, ok := fields["stream"]; !ok || json.Unmarshal(raw, &stream) != nil || !stream {
		return false
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := fields["stream_options"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &opts) != nil {
			return false
		}
		var include bool
		if v, ok := opts["include_usage"]; ok && json.Unmarshal(v, &include) == nil && include {
			return false
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	b, err := json.Marshal(opts)
	if err != nil {
		return false
	}
	fields["stream_options"] = b
	return true
}

// IsUsageOnlyChunk reports whether one SSE frame (separator stripped) is the
// extra chunk stream_options.include_usage adds before [DONE]: an empty
// choices array and a usage object. Frames with content are rejected by a
// byte scan before any JSON is parsed. A frame holding several events counts
// only when every event is one, since the caller drops the whole frame.
func IsUsageOnlyChunk(frame []byte) bool {
	events := 0
	for sc := sse.NewScanner(frame); sc.Next(); events++ {
		if !isUsageOnlyData(sc.Data()) {
			return false
		}
	}
	return events > 0
}

func isUsageOnlyData(data []byte) bool {
	if !hasEmptyChoices(data) {
		return false
	}
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return false
	}
	return len(chunk.Choices) == 0 && len(chunk.Usage) > 0 && !bytes.Equal(chunk.Usage, []byte("null"))
}

// hasEmptyChoices reports whether data holds `"choices"` followed by an
// empty array, allowing JSON whitespace around the colon and inside the brackets.
func hasEmptyChoices(data []byte) bool {
	const key = `"choices"`
	for rest := data; ; {
		i := bytes.Index(rest, []byte(key))
		if i < 0 {
			return false
		}
		rest = rest[i+len(key):]
		v := bytes.TrimLeft(rest, " \t\r\n")
		if len(v) == 0 || v[0] != ':' {
			continue
		}
		v = bytes.TrimLeft(v[1:], " \t\r\n")
		if len(v) == 0 || v[0] != '[' {
			continue
		}
		v = bytes.TrimLeft(v[1:], " \t\r\n")
		if len(v) > 0 && v[0] == ']' {
			return true
		}
	}
}
