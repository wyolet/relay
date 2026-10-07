package client

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/sdk/telemetry"
)

// exportSink is a fake OTLP endpoint that keeps each span's and event's attributes, values flattened to text.
type exportSink struct {
	mu     sync.Mutex
	spans  []map[string]string
	events []map[string]string
	srv    *httptest.Server
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string          `json:"stringValue"`
		IntValue    *string          `json:"intValue"`
		BoolValue   *bool            `json:"boolValue"`
		DoubleValue *float64         `json:"doubleValue"`
		ArrayValue  *json.RawMessage `json:"arrayValue"`
		KvlistValue *json.RawMessage `json:"kvlistValue"`
	} `json:"value"`
}

func flatten(attrs []otlpAttr) map[string]string {
	out := map[string]string{}
	for _, a := range attrs {
		v := a.Value
		switch {
		case v.StringValue != nil:
			out[a.Key] = *v.StringValue
		case v.IntValue != nil:
			out[a.Key] = *v.IntValue
		case v.BoolValue != nil:
			out[a.Key] = fmt.Sprint(*v.BoolValue)
		case v.DoubleValue != nil:
			out[a.Key] = fmt.Sprint(*v.DoubleValue)
		case v.ArrayValue != nil:
			out[a.Key] = string(*v.ArrayValue)
		case v.KvlistValue != nil:
			out[a.Key] = string(*v.KvlistValue)
		}
	}
	return out
}

func newExportSink(t *testing.T) *exportSink {
	t.Helper()
	s := &exportSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		var body struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Attributes []otlpAttr `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []struct {
						Attributes []otlpAttr `json:"attributes"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		raw, _ := io.ReadAll(zr)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode export: %v", err)
		}
		s.mu.Lock()
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					s.spans = append(s.spans, flatten(sp.Attributes))
				}
			}
		}
		for _, rl := range body.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					s.events = append(s.events, flatten(lr.Attributes))
				}
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *exportSink) emitter(t *testing.T, content bool) *telemetry.Emitter {
	t.Helper()
	e := telemetry.New(telemetry.WithEndpoint(s.srv.URL+"/otlp"), telemetry.WithAPIKey("rk"), telemetry.WithContent(content))
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// one flushes e and returns the single span recorded, failing unless exactly one was.
func (s *exportSink) one(t *testing.T, e *telemetry.Emitter) map[string]string {
	t.Helper()
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spans) != 1 {
		t.Fatalf("recorded %d calls, want 1: %v", len(s.spans), s.spans)
	}
	return s.spans[0]
}

const ccSync = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-2024-08-06","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":1024},"completion_tokens_details":{"reasoning_tokens":128}}}`

var ccStream = []string{
	`{"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
	`{"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
	`{"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":7,"total_tokens":57}}`,
}

// fakeProvider answers the Chat Completions shape: JSON, or SSE when the request asks to stream. status other than 200 answers an error.
func fakeProvider(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`))
			return
		}
		if !req.Stream {
			_, _ = w.Write([]byte(ccSync))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range ccStream {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// catalogClient is a For-resolved client pointed at the fake provider.
func catalogClient(t *testing.T, provider string, opts ...Option) *Client {
	t.Helper()
	return forFromCatalog(t, testCatalog(t), "gpt-4o", "sk-provider", WithBaseURL(provider), WithClient(opts...))
}

func TestTelemetryOffLeavesCallsUnchanged(t *testing.T) {
	provider := fakeProvider(t, http.StatusOK)
	sink := newExportSink(t)
	plain, err := catalogClient(t, provider.URL).Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	reported, err := catalogClient(t, provider.URL, WithTelemetry(sink.emitter(t, true))).Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(plain.Response)
	b, _ := json.Marshal(reported.Response)
	if string(a) != string(b) {
		t.Errorf("responses differ with telemetry on:\n%s\n%s", a, b)
	}
}

func TestGenerateRecordsOneCall(t *testing.T) {
	provider := fakeProvider(t, http.StatusOK)
	sink := newExportSink(t)
	e := sink.emitter(t, true)
	ctx := telemetry.ContextWithConversation(context.Background(), "conv-7")
	if _, err := catalogClient(t, provider.URL, WithTelemetry(e)).Generate(ctx, sampleReq()); err != nil {
		t.Fatal(err)
	}
	got := sink.one(t, e)
	want := map[string]string{
		"gen_ai.operation.name":                "chat",
		"gen_ai.provider.name":                 "openai",
		"wyolet.relay.host":                    "openai-direct",
		"gen_ai.request.model":                 "gpt-4o-2024-08-06",
		"gen_ai.request.stream":                "false",
		"gen_ai.response.model":                "gpt-4o-2024-08-06",
		"gen_ai.response.id":                   "chatcmpl-1",
		"gen_ai.conversation.id":               "conv-7",
		"http.response.status_code":            "200",
		"gen_ai.usage.input_tokens":            "1200",
		"gen_ai.usage.cache_read.input_tokens": "1024",
		"gen_ai.usage.output_tokens":           "300",
		"gen_ai.usage.reasoning.output_tokens": "128",
		"server.address":                       strings.TrimPrefix(provider.URL, "http://"),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, failed := got["error.type"]; failed {
		t.Errorf("error.type = %q on a successful call", got["error.type"])
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || !strings.Contains(sink.events[0]["gen_ai.input.messages"], "hi") || !strings.Contains(sink.events[0]["gen_ai.output.messages"], "hello") {
		t.Errorf("content events = %v, want one with the input and output", sink.events)
	}
}

func TestStreamToEOFRecordsOneCall(t *testing.T) {
	provider := fakeProvider(t, http.StatusOK)
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	drain(t, catalogClient(t, provider.URL, WithTelemetry(e)))
	got := sink.one(t, e)
	for k, v := range map[string]string{
		"gen_ai.request.stream":          "true",
		"gen_ai.response.id":             "chatcmpl-2",
		"gen_ai.response.finish_reasons": `{"values":[{"stringValue":"stop"}]}`,
		"gen_ai.usage.input_tokens":      "50",
		"gen_ai.usage.output_tokens":     "7",
		"http.response.status_code":      "200",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if got["gen_ai.response.time_to_first_chunk"] == "" || got["error.type"] != "" {
		t.Errorf("first chunk %q error %q, want a first-chunk time and no error", got["gen_ai.response.time_to_first_chunk"], got["error.type"])
	}
}

func TestStreamClosedEarlyIsMarkedCanceled(t *testing.T) {
	provider := fakeProvider(t, http.StatusOK)
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	s, err := catalogClient(t, provider.URL, WithTelemetry(e)).GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = s.Close()
	got := sink.one(t, e)
	if got["error.type"] != telemetry.ErrorCanceled || got["gen_ai.response.finish_reasons"] != `{"values":[{"stringValue":"error"}]}` {
		t.Errorf("error.type %q finish %q, want a canceled call with finish reason error", got["error.type"], got["gen_ai.response.finish_reasons"])
	}
}

func TestErrorStatusIsRecorded(t *testing.T) {
	provider := fakeProvider(t, http.StatusTooManyRequests)
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	c := catalogClient(t, provider.URL, WithTelemetry(e))
	if _, err := c.Generate(context.Background(), sampleReq()); err == nil {
		t.Fatal("want the 429")
	}
	got := sink.one(t, e)
	if got["http.response.status_code"] != "429" || got["error.type"] != "429" {
		t.Errorf("status %q error %q, want 429 both", got["http.response.status_code"], got["error.type"])
	}
	if _, err := c.GenerateStream(context.Background(), sampleReq()); err == nil {
		t.Fatal("want the 429 on the stream")
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.spans) != 2 || sink.spans[1]["error.type"] != "429" || sink.spans[1]["gen_ai.request.stream"] != "true" {
		t.Errorf("stream failure = %v", sink.spans)
	}
}

func TestNetworkErrorIsRecordedAsUnreachable(t *testing.T) {
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	if _, err := catalogClient(t, "http://127.0.0.1:1", WithTelemetry(e)).Generate(context.Background(), sampleReq()); err == nil {
		t.Fatal("want a dial error")
	}
	got := sink.one(t, e)
	if got["error.type"] != telemetry.ErrorUnreachable || got["http.response.status_code"] != "" {
		t.Errorf("error %q status %q, want unreachable with no status", got["error.type"], got["http.response.status_code"])
	}
}

func TestTimeoutIsRecorded(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := catalogClient(t, slow.URL, WithTelemetry(e)).Generate(ctx, sampleReq()); err == nil {
		t.Fatal("want a timeout")
	}
	if got := sink.one(t, e); got["error.type"] != telemetry.ErrorTimeout {
		t.Errorf("error.type = %q, want timeout", got["error.type"])
	}
}

func TestRelayClientRejectsTelemetry(t *testing.T) {
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	for name, c := range map[string]*Client{
		"Relay":   Relay("http://relay.invalid", "rk", WithTelemetry(e)),
		"RelayWS": RelayWS("http://relay.invalid", "rk", WithTelemetry(e)),
	} {
		_, err := c.Generate(context.Background(), sampleReq())
		if !errors.Is(err, errTelemetryThroughRelay) {
			t.Errorf("%s: err = %v, want the telemetry config error", name, err)
		}
	}
}

func TestMisconfiguredEmitterFailsTheFirstCall(t *testing.T) {
	t.Setenv(telemetry.EnvOTLPEndpoint, "")
	t.Setenv(telemetry.EnvRelayBaseURL, "")
	e := telemetry.New()
	_, err := catalogClient(t, "http://127.0.0.1:1", WithTelemetry(e)).Generate(context.Background(), sampleReq())
	if err == nil || err != e.Err() {
		t.Errorf("err = %v, want the emitter's config error", err)
	}
}

func TestContentIsNotBuiltWhileItWouldNotBeSent(t *testing.T) {
	provider := fakeProvider(t, http.StatusOK)
	sink := newExportSink(t)
	e := sink.emitter(t, false)
	if _, err := catalogClient(t, provider.URL, WithTelemetry(e)).Generate(context.Background(), sampleReq()); err != nil {
		t.Fatal(err)
	}
	sink.one(t, e)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 0 {
		t.Errorf("content sent without opt-in or relay's answer: %v", sink.events)
	}
}
