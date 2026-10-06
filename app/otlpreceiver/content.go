package otlpreceiver

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/payload"
)

// buildPayload turns the content reported with a call into the payload record of its usage event: the input side as the request body, the output side as the response body, each one JSON object keyed by the convention's attribute names. The owner fields repeat the event's, which is what lets the log read path join the two.
func buildPayload(reporter *lifecycle.Context, c reported, received time.Time, maxBytes int) payload.Record {
	ts := c.inf.Start
	if ts.IsZero() {
		ts = received
	}
	request, requestCut := clip(contentJSON(c.inf.Content.Input), maxBytes)
	response, responseCut := clip(contentJSON(c.inf.Content.Output), maxBytes)
	return payload.Record{
		RequestID:         c.id.RequestID(),
		Timestamp:         ts,
		ProjectID:         reporter.ProjectID,
		PrincipalID:       reporter.PrincipalID,
		RelayKeyHash:      reporter.RelayKeyHash,
		RequestBody:       request,
		ResponseBody:      response,
		RequestTruncated:  requestCut,
		ResponseTruncated: responseCut,
	}
}

// contentJSON encodes one side of a call's content, nil when the side is empty.
func contentJSON(side map[string]any) []byte {
	if len(side) == 0 {
		return nil
	}
	structured := make(map[string]any, len(side))
	for name, v := range side {
		structured[name] = structure(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Content is prose and markup; the default escaping of <, > and & would only make it harder to read.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(structured); err != nil {
		return nil
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// structure returns a content value as structured data. Exporters put content on spans as JSON text and on events as structured values; decoding the text makes both store the same bytes, so a store that keeps each distinct message once recognises a message whichever way it was reported. Text that is not a JSON array or object stays the string it is.
func structure(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if t := strings.TrimSpace(s); !strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "{") {
		return s
	}
	dec := json.NewDecoder(strings.NewReader(s))
	// Keeps integers beyond float64 precision as written.
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return s
	}
	if _, err := dec.Token(); err != io.EOF {
		return s
	}
	return plainNumbers(out)
}

// plainNumbers rewrites the numbers of decoded JSON text that are written with a fraction or an exponent as floats, in place. A structured value carries such a number as a double, which encodes in the shortest form (1.0 as 1); the text form must come out the same.
func plainNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			if f, err := x.Float64(); err == nil {
				return f
			}
		}
	case []any:
		for i, e := range x {
			x[i] = plainNumbers(e)
		}
	case map[string]any:
		for k, e := range x {
			x[k] = plainNumbers(e)
		}
	}
	return v
}

// clip cuts b to limit bytes and reports whether it did. limit <= 0 means no cap.
func clip(b []byte, limit int) ([]byte, bool) {
	if limit > 0 && len(b) > limit {
		return b[:limit], true
	}
	return b, false
}
