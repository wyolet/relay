package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	traceHex  = "5b8efff798038103d269b633813fc60c"
	spanHex   = "eee19b7ec3c1b174"
	parentHex = "eee19b7ec3c1b173"
)

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestDecodeTracesProtobuf(t *testing.T) {
	start := time.Date(2026, 8, 4, 10, 52, 22, 30_000_000, time.UTC)
	data := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "billing-agent")}},
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope: &commonpb.InstrumentationScope{Name: "example.instrumentation"},
			Spans: []*tracepb.Span{{
				TraceId:           mustHex(t, traceHex),
				SpanId:            mustHex(t, spanHex),
				ParentSpanId:      mustHex(t, parentHex),
				Name:              "chat example-model",
				StartTimeUnixNano: uint64(start.UnixNano()),
				EndTimeUnixNano:   uint64(start.Add(1500 * time.Millisecond).UnixNano()),
				Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "rate limited"},
				Attributes: []*commonpb.KeyValue{
					strAttr("gen_ai.operation.name", "chat"),
					intAttr("gen_ai.usage.input_tokens", 120),
					{Key: "gen_ai.response.finish_reasons", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
						{Value: &commonpb.AnyValue_StringValue{StringValue: "stop"}},
					}}}}},
					{Key: "gen_ai.response.time_to_first_chunk", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}}},
					{Key: "gen_ai.request.stream", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
				},
			}},
		}},
	}}}
	body, err := proto.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}

	spans, err := DecodeTraces(MediaTypeProtobuf, body)
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.TraceID != traceHex || s.SpanID != spanHex || s.ParentSpanID != parentHex {
		t.Errorf("ids = %q %q %q", s.TraceID, s.SpanID, s.ParentSpanID)
	}
	if !s.Start.Equal(start) || s.End.Sub(s.Start) != 1500*time.Millisecond {
		t.Errorf("times = %v .. %v", s.Start, s.End)
	}
	if !s.Failed || s.StatusMessage != "rate limited" {
		t.Errorf("status = %v %q", s.Failed, s.StatusMessage)
	}
	if s.Scope != "example.instrumentation" || s.Resource.Str("service.name") != "billing-agent" {
		t.Errorf("scope/resource = %q %v", s.Scope, s.Resource)
	}
	if n, ok := s.Attrs.Int("gen_ai.usage.input_tokens"); !ok || n != 120 {
		t.Errorf("input tokens = %d %v", n, ok)
	}
	if got := s.Attrs.Strings("gen_ai.response.finish_reasons"); !reflect.DeepEqual(got, []string{"stop"}) {
		t.Errorf("finish reasons = %v", got)
	}
	if f, ok := s.Attrs.Float("gen_ai.response.time_to_first_chunk"); !ok || f != 0.25 {
		t.Errorf("first chunk = %v %v", f, ok)
	}
	if b, ok := s.Attrs.Bool("gen_ai.request.stream"); !ok || !b {
		t.Errorf("stream = %v %v", b, ok)
	}
}

// OTLP/JSON differs from protobuf JSON: ids are hex, enums are integers and 64-bit integers are decimal strings.
func TestDecodeTracesJSON(t *testing.T) {
	body := `{"resourceSpans":[{
	  "resource":{"attributes":[{"key":"service.name","value":{"stringValue":"billing-agent"}}]},
	  "scopeSpans":[{"scope":{"name":"example.instrumentation"},"spans":[{
	    "traceId":"5B8EFFF798038103D269B633813FC60C",
	    "spanId":"eee19b7ec3c1b174",
	    "parentSpanId":"",
	    "name":"chat example-model",
	    "kind":3,
	    "startTimeUnixNano":"1785840742030000000",
	    "endTimeUnixNano":"1785840743530000000",
	    "status":{"code":2},
	    "unknownField":{"ignored":true},
	    "attributes":[
	      {"key":"gen_ai.usage.input_tokens","value":{"intValue":"120"}},
	      {"key":"gen_ai.usage.output_tokens","value":{"intValue":45}},
	      {"key":"gen_ai.request.model","value":{"stringValue":"example-model"}}
	    ]}]}]}]}`

	spans, err := DecodeTraces(MediaTypeJSON, []byte(body))
	if err != nil {
		t.Fatalf("DecodeTraces: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.TraceID != traceHex || s.SpanID != spanHex || s.ParentSpanID != "" {
		t.Errorf("ids = %q %q %q", s.TraceID, s.SpanID, s.ParentSpanID)
	}
	if s.End.Sub(s.Start) != 1500*time.Millisecond || !s.Failed {
		t.Errorf("duration = %v failed = %v", s.End.Sub(s.Start), s.Failed)
	}
	in, _ := s.Attrs.Int("gen_ai.usage.input_tokens")
	out, _ := s.Attrs.Int("gen_ai.usage.output_tokens")
	if in != 120 || out != 45 {
		t.Errorf("tokens = %d/%d", in, out)
	}
}

func TestDecodeTracesRejectsGarbage(t *testing.T) {
	if _, err := DecodeTraces(MediaTypeJSON, []byte(`{"resourceSpans":`)); err == nil {
		t.Error("truncated json: want error")
	}
	if _, err := DecodeTraces(MediaTypeProtobuf, []byte{0xff, 0xff, 0xff}); err == nil {
		t.Error("bad protobuf: want error")
	}
	if _, err := DecodeTraces("text/plain", nil); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Errorf("text/plain: err = %v", err)
	}
}

func TestMediaType(t *testing.T) {
	for header, want := range map[string]string{
		"application/x-protobuf":          MediaTypeProtobuf,
		"application/json; charset=utf-8": MediaTypeJSON,
		"Application/JSON":                MediaTypeJSON,
	} {
		if got, err := MediaType(header); err != nil || got != want {
			t.Errorf("MediaType(%q) = %q, %v", header, got, err)
		}
	}
	for _, header := range []string{"", "text/plain", "application/grpc"} {
		if _, err := MediaType(header); !errors.Is(err, ErrUnsupportedMediaType) {
			t.Errorf("MediaType(%q): err = %v", header, err)
		}
	}
}

func TestAttrsAccessors(t *testing.T) {
	a := Attrs{
		"int": int64(7), "float": 3.0, "frac": 3.5, "numstr": "42", "str": "x", "empty": "",
		"list": []any{"a", int64(1), "b"},
	}
	if n, ok := a.Int("missing", "float"); !ok || n != 3 {
		t.Errorf("Int(float) = %d %v", n, ok)
	}
	if _, ok := a.Int("frac"); ok {
		t.Error("Int(frac): a fractional double is not an integer")
	}
	if n, ok := a.Int("numstr"); !ok || n != 42 {
		t.Errorf("Int(numstr) = %d %v", n, ok)
	}
	if got := a.Str("empty", "str"); got != "x" {
		t.Errorf("Str skips empty: %q", got)
	}
	if got := a.Strings("list"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Strings = %v", got)
	}
	if got := a.Strings("str"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("Strings(lone string) = %v", got)
	}
	if f, ok := a.Float("int"); !ok || f != 7 {
		t.Errorf("Float(int) = %v %v", f, ok)
	}
}

func TestReadBody(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte(strings.Repeat("a", 4096)))
	_ = zw.Close()

	b, err := ReadBody(bytes.NewReader(zipped.Bytes()), "gzip", 8192)
	if err != nil || len(b) != 4096 {
		t.Fatalf("gzip: len=%d err=%v", len(b), err)
	}
	// The limit applies to the decompressed size, not the bytes on the wire.
	if _, err := ReadBody(bytes.NewReader(zipped.Bytes()), "gzip", 1024); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("gzip over limit: err = %v", err)
	}
	if _, err := ReadBody(strings.NewReader("x"), "br", 0); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("br: err = %v", err)
	}
	if b, err := ReadBody(strings.NewReader("plain"), "", 5); err != nil || string(b) != "plain" {
		t.Errorf("identity at limit: %q %v", b, err)
	}
	if _, err := ReadBody(strings.NewReader("not gzip"), "gzip", 0); err == nil {
		t.Error("corrupt gzip: want error")
	}
}

func TestTraceResponse(t *testing.T) {
	if b := TraceResponse(MediaTypeProtobuf, 0, ""); len(b) != 0 {
		t.Errorf("protobuf success = %x, want empty message", b)
	}
	if b := TraceResponse(MediaTypeJSON, 0, ""); string(b) != "{}" {
		t.Errorf("json success = %s", b)
	}

	var got struct {
		PartialSuccess struct {
			RejectedSpans string `json:"rejectedSpans"`
			ErrorMessage  string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(TraceResponse(MediaTypeJSON, 3, "no ids"), &got); err != nil {
		t.Fatal(err)
	}
	if got.PartialSuccess.RejectedSpans != "3" || got.PartialSuccess.ErrorMessage != "no ids" {
		t.Errorf("json partial = %+v", got)
	}

	b := TraceResponse(MediaTypeProtobuf, 3, "no ids")
	num, typ, n := protowire.ConsumeTag(b)
	if num != 1 || typ != protowire.BytesType {
		t.Fatalf("outer tag = %d/%d", num, typ)
	}
	partial, _ := protowire.ConsumeBytes(b[n:])
	num, _, n = protowire.ConsumeTag(partial)
	rejected, m := protowire.ConsumeVarint(partial[n:])
	if num != 1 || rejected != 3 {
		t.Errorf("rejected_spans field %d = %d", num, rejected)
	}
	num, _, k := protowire.ConsumeTag(partial[n+m:])
	msg, _ := protowire.ConsumeString(partial[n+m+k:])
	if num != 2 || msg != "no ids" {
		t.Errorf("error_message field %d = %q", num, msg)
	}
}

func TestStatusBody(t *testing.T) {
	var got struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(StatusBody(MediaTypeJSON, StatusInvalidArgument, "bad body"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != 3 || got.Message != "bad body" {
		t.Errorf("json status = %+v", got)
	}
	b := StatusBody(MediaTypeProtobuf, StatusInvalidArgument, "bad body")
	_, _, n := protowire.ConsumeTag(b)
	code, m := protowire.ConsumeVarint(b[n:])
	_, _, k := protowire.ConsumeTag(b[n+m:])
	msg, _ := protowire.ConsumeString(b[n+m+k:])
	if code != 3 || msg != "bad body" {
		t.Errorf("protobuf status = %d %q", code, msg)
	}
}
