package telemetry

import (
	"encoding/json"
	"sort"
	"strconv"
)

// The OTLP/JSON request shapes, per opentelemetry-proto's JSON mapping: lowerCamelCase field names, hex trace and span ids, 64-bit integers as decimal strings, enums as numbers.

type exportTraces struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []span `json:"spans"`
}

type exportLogs struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

type scopeLogs struct {
	Scope      scope       `json:"scope"`
	LogRecords []logRecord `json:"logRecords"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// spanKindClient and statusError are the proto enum values SPAN_KIND_CLIENT and STATUS_CODE_ERROR.
const (
	spanKindClient = 3
	statusError    = 2
	// severityDebug is SEVERITY_NUMBER_DEBUG, which the conventions ask of the inference details event.
	severityDebug = 5
)

type span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes"`
	Status            *status    `json:"status,omitempty"`
}

type status struct {
	Code int `json:"code"`
}

type logRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber"`
	EventName            string     `json:"eventName"`
	TraceID              string     `json:"traceId"`
	SpanID               string     `json:"spanId"`
	Attributes           []keyValue `json:"attributes"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is the AnyValue oneof; exactly one field is set, or none for an empty value.
type anyValue struct {
	StringValue *string      `json:"stringValue,omitempty"`
	BoolValue   *bool        `json:"boolValue,omitempty"`
	IntValue    *string      `json:"intValue,omitempty"`
	DoubleValue *float64     `json:"doubleValue,omitempty"`
	ArrayValue  *arrayValue  `json:"arrayValue,omitempty"`
	KvlistValue *kvlistValue `json:"kvlistValue,omitempty"`
}

type arrayValue struct {
	Values []anyValue `json:"values"`
}

type kvlistValue struct {
	Values []keyValue `json:"values"`
}

func stringAttr(k, v string) keyValue { return keyValue{Key: k, Value: anyValue{StringValue: &v}} }

func intAttr(k string, v int64) keyValue {
	s := strconv.FormatInt(v, 10)
	return keyValue{Key: k, Value: anyValue{IntValue: &s}}
}

func boolAttr(k string, v bool) keyValue { return keyValue{Key: k, Value: anyValue{BoolValue: &v}} }

func doubleAttr(k string, v float64) keyValue {
	return keyValue{Key: k, Value: anyValue{DoubleValue: &v}}
}

func stringsAttr(k string, vs []string) keyValue {
	values := make([]anyValue, len(vs))
	for i := range vs {
		values[i] = anyValue{StringValue: &vs[i]}
	}
	return keyValue{Key: k, Value: anyValue{ArrayValue: &arrayValue{Values: values}}}
}

// valueOf converts a decoded JSON-like value (string, bool, numbers, []any, map[string]any) to an AnyValue. Map keys are sorted so equal content encodes to equal bytes.
func valueOf(v any) anyValue {
	switch x := v.(type) {
	case string:
		return anyValue{StringValue: &x}
	case bool:
		return anyValue{BoolValue: &x}
	case int:
		s := strconv.Itoa(x)
		return anyValue{IntValue: &s}
	case int64:
		s := strconv.FormatInt(x, 10)
		return anyValue{IntValue: &s}
	case float64:
		return anyValue{DoubleValue: &x}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			s := strconv.FormatInt(n, 10)
			return anyValue{IntValue: &s}
		}
		if f, err := x.Float64(); err == nil {
			return anyValue{DoubleValue: &f}
		}
		s := x.String()
		return anyValue{StringValue: &s}
	case []any:
		values := make([]anyValue, len(x))
		for i, e := range x {
			values[i] = valueOf(e)
		}
		return anyValue{ArrayValue: &arrayValue{Values: values}}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		values := make([]keyValue, len(keys))
		for i, k := range keys {
			values[i] = keyValue{Key: k, Value: valueOf(x[k])}
		}
		return anyValue{KvlistValue: &kvlistValue{Values: values}}
	}
	return anyValue{}
}

func unixNano(t int64) string { return strconv.FormatInt(t, 10) }
