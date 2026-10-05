package otlp

import (
	"fmt"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// LogRecord is one exported log record with its resource and scope flattened onto it.
type LogRecord struct {
	// TraceID and SpanID are lowercase hex, and empty on a record emitted outside a span.
	TraceID string
	SpanID  string

	// EventName names the event the record is, when it is one. Exporters older than the field carry the name in the event.name attribute instead.
	EventName string

	// Time is when the event happened; Observed is when the collecting SDK saw it. Either may be zero.
	Time     time.Time
	Observed time.Time

	// Body is the record's body converted like an attribute value, nil when absent.
	Body any

	Attrs    Attrs
	Resource Attrs
	// Scope is the instrumentation scope name.
	Scope string
}

// LogMapper recognises the log records of one telemetry convention that describe a model call.
type LogMapper interface {
	// Name identifies the convention.
	Name() string
	// MapLog reports false for a record that is not a model call under this convention. Such a record is left for the next mapper.
	MapLog(r LogRecord) (Inference, bool)
}

// DecodeLogs decodes a logs export request body in mediaType into its records.
func DecodeLogs(mediaType string, body []byte) ([]LogRecord, error) {
	// LogsData has the same single field as ExportLogsServiceRequest, so it decodes the request without the collector package and its gRPC dependencies.
	var data logspb.LogsData
	switch mediaType {
	case MediaTypeProtobuf:
		if err := proto.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("otlp: decode protobuf: %w", err)
		}
	case MediaTypeJSON:
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("otlp: decode json: %w", err)
		}
	default:
		return nil, ErrUnsupportedMediaType
	}

	fromJSON := mediaType == MediaTypeJSON
	var records []LogRecord
	for _, rl := range data.GetResourceLogs() {
		resource := attrsFrom(rl.GetResource().GetAttributes())
		for _, sl := range rl.GetScopeLogs() {
			scope := sl.GetScope().GetName()
			for _, lr := range sl.GetLogRecords() {
				rec := LogRecord{
					TraceID:   hexID(lr.GetTraceId(), traceIDBytes, fromJSON),
					SpanID:    hexID(lr.GetSpanId(), spanIDBytes, fromJSON),
					EventName: lr.GetEventName(),
					Time:      unixNano(lr.GetTimeUnixNano()),
					Observed:  unixNano(lr.GetObservedTimeUnixNano()),
					Attrs:     attrsFrom(lr.GetAttributes()),
					Resource:  resource,
					Scope:     scope,
				}
				if lr.GetBody() != nil {
					rec.Body = valueFrom(lr.GetBody())
				}
				records = append(records, rec)
			}
		}
	}
	return records, nil
}
