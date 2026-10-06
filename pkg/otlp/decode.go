package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The two encodings OTLP/HTTP defines.
const (
	MediaTypeProtobuf = "application/x-protobuf"
	MediaTypeJSON     = "application/json"
)

// ErrUnsupportedMediaType reports a Content-Type that is neither OTLP/HTTP encoding.
var ErrUnsupportedMediaType = errors.New("otlp: unsupported content type")

const (
	traceIDBytes = 16
	spanIDBytes  = 8
)

// MediaType reduces a Content-Type header to MediaTypeProtobuf or MediaTypeJSON.
func MediaType(contentType string) (string, error) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", ErrUnsupportedMediaType
	}
	switch mt {
	case MediaTypeProtobuf, MediaTypeJSON:
		return mt, nil
	}
	return "", ErrUnsupportedMediaType
}

// DecodeTraces decodes a trace export request body in mediaType into its spans.
func DecodeTraces(mediaType string, body []byte) ([]Span, error) {
	// TracesData has the same single field as ExportTraceServiceRequest, so it decodes the request without the collector package and its gRPC dependencies.
	var data tracepb.TracesData
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
	var spans []Span
	for _, rs := range data.GetResourceSpans() {
		resource := attrsFrom(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			scope := ss.GetScope().GetName()
			for _, sp := range ss.GetSpans() {
				spans = append(spans, Span{
					TraceID:       hexID(sp.GetTraceId(), traceIDBytes, fromJSON),
					SpanID:        hexID(sp.GetSpanId(), spanIDBytes, fromJSON),
					ParentSpanID:  hexID(sp.GetParentSpanId(), spanIDBytes, fromJSON),
					Name:          sp.GetName(),
					Start:         unixNano(sp.GetStartTimeUnixNano()),
					End:           unixNano(sp.GetEndTimeUnixNano()),
					Failed:        sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR,
					StatusMessage: sp.GetStatus().GetMessage(),
					Attrs:         attrsFrom(sp.GetAttributes()),
					Resource:      resource,
					Scope:         scope,
				})
			}
		}
	}
	return spans, nil
}

// hexID returns an id as lowercase hex, or "" when it is absent or malformed. OTLP/JSON writes ids as hex, which the protobuf JSON decoder reads as base64; encoding the bytes back yields the hex text the client sent.
func hexID(b []byte, size int, fromJSON bool) string {
	if fromJSON && len(b) != size {
		raw, err := hex.DecodeString(base64.StdEncoding.EncodeToString(b))
		if err != nil {
			return ""
		}
		b = raw
	}
	if len(b) != size {
		return ""
	}
	return hex.EncodeToString(b)
}

func unixNano(ns uint64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)).UTC()
}

func attrsFrom(kvs []*commonpb.KeyValue) Attrs {
	if len(kvs) == 0 {
		return nil
	}
	out := make(Attrs, len(kvs))
	for _, kv := range kvs {
		out[kv.GetKey()] = valueFrom(kv.GetValue())
	}
	return out
}

func valueFrom(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return x.BytesValue
	case *commonpb.AnyValue_ArrayValue:
		vals := x.ArrayValue.GetValues()
		out := make([]any, len(vals))
		for i, e := range vals {
			out[i] = valueFrom(e)
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		kvs := x.KvlistValue.GetValues()
		out := make(map[string]any, len(kvs))
		for _, kv := range kvs {
			out[kv.GetKey()] = valueFrom(kv.GetValue())
		}
		return out
	}
	return nil
}
