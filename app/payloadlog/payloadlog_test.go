package payloadlog

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/lifecycle"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// memSink is an in-memory Sink for observer tests.
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

// fakeReader is a settable SettingsSource. The tests drive reconcile
// directly, so OnSettingsChange just records the callback.
type fakeReader struct {
	mu      sync.Mutex
	cfg     settings.PayloadLogging
	present bool
	subs    []func()
}

func (f *fakeReader) OnSettingsChange(_ string, fn func()) {
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	f.mu.Unlock()
}

// fire invokes the registered callbacks the way the catalog NOTIFY
// listener would after a settings change.
func (f *fakeReader) fire() {
	f.mu.Lock()
	subs := append([]func(){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

func (f *fakeReader) set(c settings.PayloadLogging) {
	f.mu.Lock()
	f.cfg, f.present = c, true
	f.mu.Unlock()
}

func (f *fakeReader) Setting(string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return nil, false
	}
	c := f.cfg
	return &c, true
}

// enabledCtrl returns a Controller in the enabled state with maxBytes and a
// memSink installed — no reconcile loop running.
func enabledCtrl(maxBytes int) (*Controller, *memSink) {
	sink := &memSink{}
	c := NewController(&fakeReader{}, func(context.Context, settings.PayloadLogging) (Sink, error) { return sink, nil }, testLogger())
	c.set(true, maxBytes, settings.PayloadLogging{Enabled: true})
	c.rsink.swap(sink, c.log)
	return c, sink
}

func disabledCtrl() *Controller {
	return NewController(&fakeReader{}, func(context.Context, settings.PayloadLogging) (Sink, error) { return &memSink{}, nil }, testLogger())
}

func mustResult(t *testing.T, obs lifecycle.StreamObserver) *Record {
	t.Helper()
	v, err := obs.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	return v.(*Record)
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
	// Per-request opt-in off → nothing, even when globally enabled.
	c, _ := enabledCtrl(0)
	if v, _ := NewPayloadHook(c).Fill(ctx(false, `{"in":1}`), &lifecycle.PostFlightEvent{Status: 200}); v != nil {
		t.Fatalf("opt-in off: want nil, got %v", v)
	}
	// Globally disabled → nothing, even when opted in.
	if v, _ := NewPayloadHook(disabledCtrl()).Fill(ctx(true, `{"in":1}`), &lifecycle.PostFlightEvent{Status: 200}); v != nil {
		t.Fatalf("globally disabled: want nil, got %v", v)
	}
	// Both on → Record with both bodies.
	v, err := NewPayloadHook(c).Fill(ctx(true, `{"in":1}`), &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	r := v.(*Record)
	if string(r.RequestBody) != `{"in":1}` || string(r.ResponseBody) != `{"out":2}` {
		t.Fatalf("record: %+v", r)
	}
}

func TestHook_Truncation(t *testing.T) {
	c, _ := enabledCtrl(4)
	v, _ := NewPayloadHook(c).Fill(ctx(true, "0123456789"), &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte("abcdefgh")})
	r := v.(*Record)
	if string(r.RequestBody) != "0123" || !r.RequestTruncated || string(r.ResponseBody) != "abcd" || !r.ResponseTruncated {
		t.Fatalf("clip: %+v", r)
	}
}

func TestStreamObserver_Gating(t *testing.T) {
	if _, ok := NewStreamPayloadFactory(disabledCtrl()).NewObserver(ctx(true, "")).(noopObserver); !ok {
		t.Fatal("disabled: want noopObserver")
	}
	c, _ := enabledCtrl(0)
	if _, ok := NewStreamPayloadFactory(c).NewObserver(ctx(false, "")).(noopObserver); !ok {
		t.Fatal("opt-in off: want noopObserver")
	}
	obs := NewStreamPayloadFactory(c).NewObserver(ctx(true, `{"in":1}`))
	obs.Observe([]byte("data: a"))
	obs.Observe([]byte("data: b"))
	r := mustResult(t, obs)
	if string(r.ResponseBody) != "data: a\n\ndata: b\n\n" || string(r.RequestBody) != `{"in":1}` {
		t.Fatalf("stream record: %+v", r)
	}
}

func TestRegistryChain(t *testing.T) {
	c, sink := enabledCtrl(0)
	reg := lifecycle.New()
	reg.RegisterHook(NewPayloadHook(c))
	reg.RegisterCollector(NewSinkCollector(c.Emitter()))
	reg.Finalize(context.Background(), ctx(true, `{"in":1}`),
		&lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte(`{"out":2}`)})
	c.Close() // drains

	if sink.count() != 1 || sink.recs[0].RequestID != "req-1" {
		t.Fatalf("want 1 record req-1, got %+v", sink.recs)
	}
}

func TestController_Reconcile(t *testing.T) {
	reader := &fakeReader{}
	var built int
	sinks := map[string]*memSink{}
	build := func(_ context.Context, cfg settings.PayloadLogging) (Sink, error) {
		built++
		s := &memSink{}
		sinks[cfg.S3.Bucket] = s
		return s, nil
	}
	c := NewController(reader, build, testLogger())

	// No settings row yet → reconcile leaves it disabled, no build.
	c.reconcile(context.Background())
	if c.Enabled() || built != 0 {
		t.Fatalf("no-row: enabled=%v built=%d", c.Enabled(), built)
	}

	// Enable with bucket A → builds + enabled, maxBytes applied.
	reader.set(settings.PayloadLogging{Enabled: true, Backend: "s3", MaxBytes: 7, S3: settings.PayloadS3{Bucket: "A"}})
	c.reconcile(context.Background())
	if !c.Enabled() || c.MaxBytes() != 7 || built != 1 {
		t.Fatalf("enable A: enabled=%v max=%d built=%d", c.Enabled(), c.MaxBytes(), built)
	}

	// Same config again → no rebuild (idempotent).
	c.reconcile(context.Background())
	if built != 1 {
		t.Fatalf("idempotent: built=%d", built)
	}

	// Change bucket → hot-swap (rebuild).
	reader.set(settings.PayloadLogging{Enabled: true, Backend: "s3", MaxBytes: 7, S3: settings.PayloadS3{Bucket: "B"}})
	c.reconcile(context.Background())
	if built != 2 {
		t.Fatalf("swap: built=%d", built)
	}

	// Disable → teardown, no further build, gate off.
	reader.set(settings.PayloadLogging{Enabled: false})
	c.reconcile(context.Background())
	if c.Enabled() || built != 2 {
		t.Fatalf("disable: enabled=%v built=%d", c.Enabled(), built)
	}
}

func TestController_BuildErrorKeepsPrevious(t *testing.T) {
	reader := &fakeReader{}
	fail := false
	c := NewController(reader, func(context.Context, settings.PayloadLogging) (Sink, error) {
		if fail {
			return nil, io.ErrUnexpectedEOF
		}
		return &memSink{}, nil
	}, testLogger())

	reader.set(settings.PayloadLogging{Enabled: true, Backend: "file"})
	c.reconcile(context.Background())
	if !c.Enabled() {
		t.Fatal("initial enable failed")
	}
	// New config that fails to build → stays enabled on the previous sink,
	// applied unchanged so it retries.
	fail = true
	reader.set(settings.PayloadLogging{Enabled: true, Backend: "file", MaxBytes: 99})
	c.reconcile(context.Background())
	if !c.Enabled() || c.MaxBytes() == 99 {
		t.Fatalf("build error should keep previous: enabled=%v max=%d", c.Enabled(), c.MaxBytes())
	}
}

func TestController_SignalDrivenRun(t *testing.T) {
	reader := &fakeReader{}
	c := NewController(reader, func(context.Context, settings.PayloadLogging) (Sink, error) {
		return &memSink{}, nil
	}, testLogger())

	c.Subscribe()
	reader.mu.Lock()
	nSubs := len(reader.subs)
	reader.mu.Unlock()
	if nSubs != 1 {
		t.Fatalf("Subscribe registered %d callbacks, want 1", nSubs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Enable via settings, then fire the change callback as the catalog
	// would. Run must pick it up off the signal and reconcile to enabled.
	reader.set(settings.PayloadLogging{Enabled: true, Backend: "file"})
	reader.fire()

	deadline := time.After(2 * time.Second)
	for !c.Enabled() {
		select {
		case <-deadline:
			t.Fatal("controller did not enable after settings-change signal")
		case <-time.After(time.Millisecond):
		}
	}
}
