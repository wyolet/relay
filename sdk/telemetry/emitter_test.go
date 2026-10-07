package telemetry

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// collector is a fake OTLP endpoint. respond picks the answer to each request; nil answers 200 with no body.
type collector struct {
	mu       sync.Mutex
	requests []received
	respond  func(path string, n int, w http.ResponseWriter) bool
	srv      *httptest.Server
}

type received struct {
	path   string
	auth   string
	traces exportTraces
	logs   exportLogs
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v, want gzip JSON", r.Header)
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		raw, _ := io.ReadAll(zr)
		got := received{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		if r.URL.Path == "/otlp/v1/traces" {
			err = json.Unmarshal(raw, &got.traces)
		} else {
			err = json.Unmarshal(raw, &got.logs)
		}
		if err != nil {
			t.Errorf("decode %s: %v", r.URL.Path, err)
		}
		c.mu.Lock()
		c.requests = append(c.requests, got)
		n := len(c.requests)
		respond := c.respond
		c.mu.Unlock()
		if respond != nil && respond(r.URL.Path, n, w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) seen() []received {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]received(nil), c.requests...)
}

// spans counts the spans received on the traces path, and calls with content on the logs path.
func (c *collector) counts() (spans, events, probes int) {
	for _, r := range c.seen() {
		for _, rs := range r.traces.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans += len(ss.Spans)
			}
		}
		if r.path == "/otlp/v1/logs" {
			if len(r.logs.ResourceLogs) == 0 {
				probes++
			}
			for _, rl := range r.logs.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					events += len(sl.LogRecords)
				}
			}
		}
	}
	return spans, events, probes
}

func newTestEmitter(t *testing.T, c *collector, opts ...Option) *Emitter {
	t.Helper()
	e := New(append([]Option{WithEndpoint(c.srv.URL + "/otlp"), WithAPIKey("rk-test"), WithServiceName("tests")}, opts...)...)
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	e.backoff = 10 * time.Millisecond
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func call(model string) Call {
	return Call{Provider: "acme", RequestModel: model, Start: time.Now(), End: time.Now(), StatusCode: 200}
}

func withContent(c Call) Call {
	c.Content = NewContent(&v1.Request{Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}}})
	return c
}

func TestFlushExportsRecordedCallsWithTheRelayKey(t *testing.T) {
	c := newCollector(t)
	e := newTestEmitter(t, c)
	e.Record(call("m1"))
	e.Record(call("m2"))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans, events, _ := c.counts()
	if spans != 2 || events != 0 {
		t.Errorf("spans %d events %d, want 2 spans and no event without content", spans, events)
	}
	for _, r := range c.seen() {
		if r.auth != "Bearer rk-test" {
			t.Errorf("%s auth = %q", r.path, r.auth)
		}
	}
	if e.Dropped() != 0 {
		t.Errorf("Dropped = %d", e.Dropped())
	}
}

func TestBatchesAreCutAtTheBatchSize(t *testing.T) {
	c := newCollector(t)
	e := newTestEmitter(t, c, WithBatch(2, time.Hour))
	for range 5 {
		e.Record(call("m"))
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, r := range c.seen() {
		if r.path == "/otlp/v1/traces" {
			sizes = append(sizes, len(r.traces.ResourceSpans[0].ScopeSpans[0].Spans))
		}
	}
	if len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Errorf("batch sizes = %v, want [2 2 1]", sizes)
	}
}

func TestIntervalExportsWithoutFlush(t *testing.T) {
	c := newCollector(t)
	e := newTestEmitter(t, c, WithBatch(100, 20*time.Millisecond))
	e.Record(call("m"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spans, _, _ := c.counts(); spans == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the interval did not export the call")
}

func TestFullQueueDropsAndCounts(t *testing.T) {
	c := newCollector(t)
	release := make(chan struct{})
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		<-release
		return false
	}
	e := newTestEmitter(t, c, WithQueueSize(2), WithBatch(1, time.Hour))
	// The first call is taken by the export goroutine and blocks on the server; two more fill the queue.
	e.Record(call("m"))
	time.Sleep(50 * time.Millisecond)
	for range 5 {
		e.Record(call("m"))
	}
	if got := e.Dropped(); got < 3 {
		t.Errorf("Dropped = %d, want at least 3 of the calls past the queue", got)
	}
	close(release)
}

func TestRecordAfterCloseIsDropped(t *testing.T) {
	c := newCollector(t)
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spans, _, _ := c.counts(); spans != 1 {
		t.Errorf("Close exported %d spans, want the queued one", spans)
	}
	e.Record(call("m"))
	if e.Dropped() != 1 {
		t.Errorf("Dropped = %d, want the call after Close", e.Dropped())
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Errorf("Flush after Close = %v", err)
	}
}

func TestRetriesHonourRetryAfter(t *testing.T) {
	c := newCollector(t)
	var first time.Time
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path != "/otlp/v1/traces" {
			return false
		}
		if first.IsZero() {
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		if time.Since(first) < 900*time.Millisecond {
			t.Errorf("retried after %v, before Retry-After", time.Since(first))
		}
		return false
	}
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	if err := e.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spans, _, _ := c.counts(); spans != 2 || e.Dropped() != 0 {
		t.Errorf("spans sent %d dropped %d, want the batch sent twice and kept", spans, e.Dropped())
	}
}

func TestRetriesStopAfterThreeAttempts(t *testing.T) {
	c := newCollector(t)
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path != "/otlp/v1/traces" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	e.Record(call("m"))
	if err := e.Flush(context.Background()); err == nil {
		t.Error("Flush = nil, want the export error")
	}
	if spans, _, _ := c.counts(); spans != 6 {
		t.Errorf("sent %d spans, want 2 calls × 3 attempts", spans)
	}
	if e.Dropped() != 2 {
		t.Errorf("Dropped = %d, want the 2 calls", e.Dropped())
	}
}

func TestRetryAfterBeyondTheBudgetDropsAtOnce(t *testing.T) {
	c := newCollector(t)
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path != "/otlp/v1/traces" {
			return false
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	start := time.Now()
	_ = e.Flush(context.Background())
	if time.Since(start) > 5*time.Second || e.Dropped() != 1 {
		t.Errorf("took %v, dropped %d; want an immediate drop", time.Since(start), e.Dropped())
	}
}

func TestClientErrorDropsWithoutRetry(t *testing.T) {
	c := newCollector(t)
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path != "/otlp/v1/traces" {
			return false
		}
		w.WriteHeader(http.StatusBadRequest)
		return true
	}
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	_ = e.Flush(context.Background())
	if spans, _, _ := c.counts(); spans != 1 || e.Dropped() != 1 {
		t.Errorf("sent %d dropped %d, want one attempt and a drop", spans, e.Dropped())
	}
}

func TestNetworkErrorIsRetriedThenDropped(t *testing.T) {
	e := New(WithEndpoint("http://127.0.0.1:1/otlp"), WithAPIKey("k"))
	e.backoff = time.Millisecond
	defer e.Close(context.Background())
	e.Record(call("m"))
	if err := e.Flush(context.Background()); err == nil || e.Dropped() != 1 {
		t.Errorf("Flush = %v dropped %d, want an error and a drop", err, e.Dropped())
	}
}

func TestPartialSuccessMessageIsKept(t *testing.T) {
	c := newCollector(t)
	c.respond = func(path string, n int, w http.ResponseWriter) bool {
		if path != "/otlp/v1/traces" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"model-call spans rejected: 1 without a trace id"}}`))
		return true
	}
	e := newTestEmitter(t, c)
	e.Record(call("m"))
	_ = e.Flush(context.Background())
	if got := e.LastWarning(); !strings.Contains(got, "rejected") {
		t.Errorf("LastWarning = %q", got)
	}
}

func TestMissingConfigSurfacesOnUse(t *testing.T) {
	t.Setenv(EnvOTLPEndpoint, "")
	t.Setenv(EnvRelayBaseURL, "")
	e := New()
	if e.Err() == nil {
		t.Fatal("Err = nil, want the missing endpoint")
	}
	e.Record(call("m"))
	if e.Dropped() != 1 || e.Flush(context.Background()) == nil || e.SendsContent() {
		t.Error("a misconfigured emitter must drop, fail Flush, and send no content")
	}
}

func TestRelayEnvironmentNeedsTheKey(t *testing.T) {
	t.Setenv(EnvOTLPEndpoint, "")
	t.Setenv(EnvRelayBaseURL, "https://relay.example/")
	t.Setenv(EnvRelayAPIKey, "")
	if New().Err() == nil {
		t.Error("Err = nil, want the missing relay key")
	}
	t.Setenv(EnvRelayAPIKey, "rk")
	e := New()
	defer e.Close(context.Background())
	if e.Err() != nil || e.cfg.endpoint != "https://relay.example/otlp" || e.cfg.headers["Authorization"] != "Bearer rk" {
		t.Errorf("err %v endpoint %q headers %v", e.Err(), e.cfg.endpoint, e.cfg.headers)
	}
}

func TestOTLPEnvironmentNeverGetsTheRelayKey(t *testing.T) {
	t.Setenv(EnvOTLPEndpoint, "https://collector.example")
	t.Setenv(EnvOTLPHeaders, "x-team=search%20infra")
	t.Setenv(EnvRelayBaseURL, "https://relay.example")
	t.Setenv(EnvRelayAPIKey, "rk")
	t.Setenv(EnvCaptureContent, "span_only")
	e := New()
	defer e.Close(context.Background())
	if e.cfg.endpoint != "https://collector.example" || e.cfg.headers["x-team"] != "search infra" {
		t.Errorf("endpoint %q headers %v", e.cfg.endpoint, e.cfg.headers)
	}
	if _, ok := e.cfg.headers["Authorization"]; ok {
		t.Error("the relay key was sent to an OTLP backend")
	}
	if !*e.cfg.content {
		t.Error("content opt-in from the environment not read")
	}
}
