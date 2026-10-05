package otlp

import (
	"encoding/json"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
)

// gRPC status codes carried in the Status body of an OTLP/HTTP error response.
const (
	StatusInvalidArgument   int32 = 3
	StatusNotFound          int32 = 5
	StatusResourceExhausted int32 = 8
	StatusUnavailable       int32 = 14
)

// TraceResponse encodes an ExportTraceServiceResponse in mediaType. rejected > 0 makes it a partial success naming how many spans were refused and why; otherwise the response is the empty success message.
func TraceResponse(mediaType string, rejected int64, message string) []byte {
	if mediaType == MediaTypeJSON {
		if rejected <= 0 {
			return []byte("{}")
		}
		// OTLP/JSON writes 64-bit integers as decimal strings.
		b, _ := json.Marshal(map[string]any{"partialSuccess": map[string]any{
			"rejectedSpans": strconv.FormatInt(rejected, 10),
			"errorMessage":  message,
		}})
		return b
	}
	if rejected <= 0 {
		return nil
	}
	// ExportTracePartialSuccess{rejected_spans = 1, error_message = 2} inside ExportTraceServiceResponse{partial_success = 1}.
	var partial []byte
	partial = protowire.AppendTag(partial, 1, protowire.VarintType)
	partial = protowire.AppendVarint(partial, uint64(rejected))
	if message != "" {
		partial = protowire.AppendTag(partial, 2, protowire.BytesType)
		partial = protowire.AppendString(partial, message)
	}
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(out, partial)
}

// StatusBody encodes the google.rpc.Status message an OTLP/HTTP error response carries, in mediaType.
func StatusBody(mediaType string, code int32, message string) []byte {
	if mediaType == MediaTypeJSON {
		b, _ := json.Marshal(map[string]any{"code": code, "message": message})
		return b
	}
	// Status{code = 1, message = 2}.
	out := protowire.AppendTag(nil, 1, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(code))
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	return protowire.AppendString(out, message)
}
