package usagelog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
)

func usageSettings(cfg settings.UsageLogging) *settingstest.Source {
	return settingstest.Sections(map[string]any{settings.SectionUsageLogging: &cfg})
}

// fakeBackend is a Sink+Reader+Closer that records closure.
type fakeBackend struct {
	name   string
	closed atomic.Bool
}

func (f *fakeBackend) Write(Event) error { return nil }
func (f *fakeBackend) Close() error      { f.closed.Store(true); return nil }
func (f *fakeBackend) Events(context.Context, EventQuery) ([]Event, error) {
	return nil, nil
}
func (f *fakeBackend) Summary(context.Context, SummaryQuery) (SummaryResult, error) {
	return SummaryResult{}, nil
}
func (f *fakeBackend) TimeSeries(context.Context, TimeSeriesQuery) (TimeSeriesResult, error) {
	return TimeSeriesResult{}, nil
}

func TestController_ReconcileSwapsBackendAndClosesOld(t *testing.T) {
	src := usageSettings(settings.UsageLogging{Backend: "file"})

	var built []*fakeBackend
	var bmu sync.Mutex
	build := func(_ context.Context, cfg settings.UsageLogging) (Backend, error) {
		be := &fakeBackend{name: cfg.Backend}
		bmu.Lock()
		built = append(built, be)
		bmu.Unlock()
		return Backend{Sink: be, Reader: be}, nil
	}

	c := NewController(src, build, testLogger())

	// Initial reconcile → first backend live.
	c.reconcile(context.Background())
	bmu.Lock()
	n := len(built)
	bmu.Unlock()
	if n != 1 || built[0].name != "file" {
		t.Fatalf("after initial reconcile: built=%d first=%q", n, built[0].name)
	}

	// Same config → no rebuild.
	c.reconcile(context.Background())
	bmu.Lock()
	n = len(built)
	bmu.Unlock()
	if n != 1 {
		t.Fatalf("same config should not rebuild, built=%d", n)
	}

	// Change backend → rebuild + old closed.
	src.Change(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "clickhouse"})
	c.reconcile(context.Background())
	bmu.Lock()
	n = len(built)
	bmu.Unlock()
	if n != 2 || built[1].name != "clickhouse" {
		t.Fatalf("after reroute: built=%d", n)
	}
	if !built[0].closed.Load() {
		t.Fatalf("old backend sink not closed on swap")
	}
	if built[1].closed.Load() {
		t.Fatalf("new backend should not be closed")
	}
}

func TestController_KeepsPreviousOnBuildError(t *testing.T) {
	src := usageSettings(settings.UsageLogging{Backend: "file"})
	good := &fakeBackend{name: "file"}
	build := func(_ context.Context, cfg settings.UsageLogging) (Backend, error) {
		if cfg.Backend == "bad" {
			return Backend{}, context.DeadlineExceeded
		}
		return Backend{Sink: good, Reader: good}, nil
	}
	c := NewController(src, build, testLogger())
	c.reconcile(context.Background())

	src.Change(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "bad"})
	c.reconcile(context.Background())
	if good.closed.Load() {
		t.Fatal("a failed rebuild must not tear down the working sink")
	}
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
