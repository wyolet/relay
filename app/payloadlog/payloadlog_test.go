package payloadlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// memSink is an in-memory payload.Sink for observer tests — keeps the
// observer suite independent of any storage backend.
type memSink struct {
	mu   sync.Mutex
	recs []Record
}

func (m *memSink) Write(r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}

func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

func ctx(payloadLog bool, reqBody string) *lifecycle.Context {
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
	lc.PayloadLog = payloadLog
	lc.PolicyID = "pol-1"
	lc.ModelID = "mod-1"
	lc.HostID = "host-1"
	if reqBody != "" {
		lc.RequestBody = []byte(reqBody)
	}
	return lc
}

func TestHook_Gating(t *testing.T) {
	h := NewPayloadHook(0)

	// Disabled → nothing produced.
	v, err := h.Fill(ctx(false, `{"in":1}`), &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	if err != nil || v != nil {
		t.Fatalf("disabled: want (nil,nil), got (%v,%v)", v, err)
	}

	// Enabled → Record with both bodies.
	v, err = h.Fill(ctx(true, `{"in":1}`), &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	r, ok := v.(*Record)
	if !ok || r == nil {
		t.Fatalf("want *Record, got %T", v)
	}
	if string(r.RequestBody) != `{"in":1}` || string(r.ResponseBody) != `{"out":2}` {
		t.Fatalf("bodies: req=%q resp=%q", r.RequestBody, r.ResponseBody)
	}
	if r.RequestTruncated || r.ResponseTruncated {
		t.Fatalf("unexpected truncation: %+v", r)
	}
	if r.PolicyID != "pol-1" || r.ModelID != "mod-1" || r.Status != 200 {
		t.Fatalf("identity/status: %+v", r)
	}
}

func TestHook_Truncation(t *testing.T) {
	h := NewPayloadHook(4) // cap each body at 4 bytes
	v, _ := h.Fill(ctx(true, "0123456789"), &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte("abcdefgh")})
	r := v.(*Record)
	if string(r.RequestBody) != "0123" || !r.RequestTruncated {
		t.Fatalf("req clip: %q trunc=%v", r.RequestBody, r.RequestTruncated)
	}
	if string(r.ResponseBody) != "abcd" || !r.ResponseTruncated {
		t.Fatalf("resp clip: %q trunc=%v", r.ResponseBody, r.ResponseTruncated)
	}
}

func TestStreamObserver_Gating(t *testing.T) {
	f := NewStreamPayloadFactory(0)

	// Disabled → no-op observer attaches nothing.
	if _, ok := f.NewObserver(ctx(false, "")).(noopObserver); !ok {
		t.Fatal("disabled stream: want noopObserver")
	}

	// Enabled → accumulates frames, restoring SSE separators.
	obs := f.NewObserver(ctx(true, `{"in":1}`))
	obs.Observe([]byte("data: a"))
	obs.Observe([]byte("data: b"))
	v, err := obs.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	r := v.(*Record)
	if string(r.RequestBody) != `{"in":1}` {
		t.Fatalf("stream req body: %q", r.RequestBody)
	}
	if string(r.ResponseBody) != "data: a\n\ndata: b\n\n" {
		t.Fatalf("stream resp body: %q", r.ResponseBody)
	}
	if r.Status != 200 {
		t.Fatalf("stream status: %d", r.Status)
	}
}

func TestRegistryChain(t *testing.T) {
	sink := &memSink{}
	em := NewEmitter(EmitterOptions{}, sink)

	// Drive the real produce→attach→collect chain through the Registry.
	reg := lifecycle.New()
	reg.RegisterHook(NewPayloadHook(0))
	reg.RegisterCollector(NewSinkCollector(em))
	reg.Finalize(context.Background(), ctx(true, `{"in":1}`),
		&lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	em.Close() // drains

	if sink.count() != 1 {
		t.Fatalf("want 1 record, got %d", sink.count())
	}
	if got := sink.recs[0]; got.RequestID != "req-1" || string(got.ResponseBody) != `{"out":2}` {
		t.Fatalf("wrong record: %+v", got)
	}

	// Disabled request produces no row.
	sink2 := &memSink{}
	em2 := NewEmitter(EmitterOptions{}, sink2)
	reg2 := lifecycle.New()
	reg2.RegisterHook(NewPayloadHook(0))
	reg2.RegisterCollector(NewSinkCollector(em2))
	reg2.Finalize(context.Background(), ctx(false, `{"in":1}`),
		&lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	em2.Close()
	if sink2.count() != 0 {
		t.Fatalf("disabled request should emit nothing, got %d", sink2.count())
	}
}
