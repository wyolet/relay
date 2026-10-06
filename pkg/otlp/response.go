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

// Signal is an OTLP signal with an export endpoint of its own.
type Signal string

const (
	SignalTraces Signal = "traces"
	SignalLogs   Signal = "logs"
)

// rejectedField is the OTLP/JSON name of a signal's count of refused records. It is the only part of the export response the signals do not share: in protobuf the count is field 1 for every signal.
func (s Signal) rejectedField() string {
	if s == SignalLogs {
		return "rejectedLogRecords"
	}
	return "rejectedSpans"
}

// ExportResponse encodes the export response of signal (ExportTraceServiceResponse, ExportLogsServiceResponse) in mediaType. rejected > 0 makes it a partial success naming how many records were refused and why. With nothing rejected, a message makes it a warning to the client about an export that was accepted in full, which OTLP carries as a partial success with a count of zero. With neither, the response is the empty success message.
func ExportResponse(mediaType string, signal Signal, rejected int64, message string) []byte {
	rejected = max(rejected, 0)
	if rejected == 0 && message == "" {
		if mediaType == MediaTypeJSON {
			return []byte("{}")
		}
		return nil
	}
	if mediaType == MediaTypeJSON {
		partial := map[string]any{"errorMessage": message}
		if rejected > 0 {
			// OTLP/JSON writes 64-bit integers as decimal strings.
			partial[signal.rejectedField()] = strconv.FormatInt(rejected, 10)
		}
		b, _ := json.Marshal(map[string]any{"partialSuccess": partial})
		return b
	}
	// Export<Signal>PartialSuccess{rejected = 1, error_message = 2} inside Export<Signal>ServiceResponse{partial_success = 1}. A zero count is the field's default and is left off the wire.
	var partial []byte
	if rejected > 0 {
		partial = protowire.AppendTag(partial, 1, protowire.VarintType)
		partial = protowire.AppendVarint(partial, uint64(rejected))
	}
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
