package inference

// The request handling ParsedRequest replaced — a probe struct decode, then a
// round trip through map[string]json.RawMessage for each edit — kept as the
// reference the one-pass path is compared and benchmarked against.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// probeModelStream reads model and stream with a probe decode, then refuses a duplicate model with the scanner.
func probeModelStream(body []byte) (model string, stream bool, err error) {
	var probe struct {
		Model      string `json:"model"`
		Stream     *bool  `json:"stream"`
		OutputMode string `json:"output_mode"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false, fmt.Errorf("invalid JSON: %w", err)
	}
	var scan modelScanner
	if scan.feed(body); scan.err == errDuplicateModel {
		return "", false, errDuplicateModel
	}
	s := probe.OutputMode == "stream"
	if probe.Stream != nil {
		s = *probe.Stream
	}
	return probe.Model, s, nil
}

// probeCode is the API error code the probe path answered body with, "" when it was accepted.
func probeCode(body []byte) string {
	m, _, err := probeModelStream(body)
	switch {
	case err == errDuplicateModel:
		return errDuplicateModel.code
	case err != nil:
		return "parse_error"
	case m == "":
		return "missing_model"
	}
	return ""
}

// rewriteViaMap sets the model and applies edit in one decode/encode of the top-level members.
func rewriteViaMap(body []byte, newName string, edit func(map[string]json.RawMessage) bool) (out []byte, edited bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, false
	}
	if edit != nil {
		edited = edit(fields)
	}
	cur, ok := fields["model"]
	if ok {
		var s string
		if err := json.Unmarshal(cur, &s); err == nil && s == newName && !edited {
			return body, false
		}
	}
	fields["model"] = json.RawMessage(strconv.Quote(newName))
	var buf bytes.Buffer
	buf.Grow(len(body) + len(newName))
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return body, false
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), edited
}

// stripViaMap removes unsupported params at their paths, re-encoding only when something was removed.
func stripViaMap(body []byte, unsupported []string, paths map[string]string) ([]byte, []string) {
	if len(paths) == 0 {
		return body, nil
	}
	var fields map[string]json.RawMessage
	var dropped []string
	for _, p := range unsupported {
		path, ok := paths[p]
		if !ok {
			continue
		}
		if fields == nil {
			if err := json.Unmarshal(body, &fields); err != nil {
				return body, nil
			}
		}
		if deleteViaMap(fields, strings.Split(path, ".")) {
			dropped = append(dropped, p)
		}
	}
	if len(dropped) == 0 {
		return body, nil
	}
	out, err := encodeMap(fields, len(body))
	if err != nil {
		return body, nil
	}
	return out, dropped
}

func deleteViaMap(fields map[string]json.RawMessage, segs []string) bool {
	if len(segs) == 1 {
		if _, ok := fields[segs[0]]; !ok {
			return false
		}
		delete(fields, segs[0])
		return true
	}
	raw, ok := fields[segs[0]]
	if !ok {
		return false
	}
	var sub map[string]json.RawMessage
	if json.Unmarshal(raw, &sub) != nil {
		return false
	}
	if !deleteViaMap(sub, segs[1:]) {
		return false
	}
	nb, err := encodeMap(sub, len(raw))
	if err != nil {
		return false
	}
	fields[segs[0]] = nb
	return true
}

func encodeMap(fields map[string]json.RawMessage, sizeHint int) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(sizeHint)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// streamUsageViaMap is the Chat Completions usage opt-in as it edited the decoded members.
func streamUsageViaMap(fields map[string]json.RawMessage) bool {
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

// bytePassViaMap is what byte-pass sent upstream: model rewrite with the optional usage opt-in, then the param strip.
func bytePassViaMap(body []byte, upstreamModel string, usageOptIn bool, unsupported []string, paths map[string]string) (out []byte, usage bool, dropped []string) {
	var edit func(map[string]json.RawMessage) bool
	if usageOptIn {
		edit = streamUsageViaMap
	}
	out, usage = rewriteViaMap(body, upstreamModel, edit)
	if len(unsupported) > 0 {
		out, dropped = stripViaMap(out, unsupported, paths)
	}
	return out, usage, dropped
}
