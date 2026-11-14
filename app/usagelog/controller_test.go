package usagelog

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
)

func usageSettings(backend string) *settingstest.Source {
	return settingstest.Sections(map[string]any{settings.SectionUsageLogging: &settings.UsageLogging{Backend: backend}})
}

// fakeBackend is a Sink+Reader+Closer. Every read answers an error naming the
// backend and the method, so a delegated call is recognisable.
type fakeBackend struct {
	name string

	mu      sync.Mutex
	written []string
	closed  int
}

func (f *fakeBackend) Write(ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, ev.RequestID)
	return nil
}

func (f *fakeBackend) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeBackend) state() (written []string, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.written), f.closed
}

func (f *fakeBackend) answer(method string) error { return errors.New(f.name + "." + method) }

func (f *fakeBackend) Events(context.Context, EventQuery) ([]Event, error) {
	return nil, f.answer("Events")
}
func (f *fakeBackend) Count(context.Context, EventQuery, int) (int64, error) {
	return 0, f.answer("Count")
}
func (f *fakeBackend) Summary(context.Context, SummaryQuery) (SummaryResult, error) {
	return SummaryResult{}, f.answer("Summary")
}
func (f *fakeBackend) TimeSeries(context.Context, TimeSeriesQuery) (TimeSeriesResult, error) {
	return TimeSeriesResult{}, f.answer("TimeSeries")
}
func (f *fakeBackend) Facet(context.Context, FacetQuery) ([]FacetValue, error) {
	return nil, f.answer("Facet")
}

// backends builds a fakeBackend per config and records them; "bad" fails to build.
type backends struct {
	mu    sync.Mutex
	built []*fakeBackend
}

func (b *backends) build(_ context.Context, cfg settings.UsageLogging) (Backend, error) {
	if cfg.Backend == "bad" {
		return Backend{}, context.DeadlineExceeded
	}
	be := &fakeBackend{name: cfg.Backend}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.built = append(b.built, be)
	return Backend{Sink: be, Reader: be}, nil
}

func (b *backends) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, be := range b.built {
		out = append(out, be.name)
	}
	return out
}

// readAnswers calls every Reader method and returns the errors, in method order.
func readAnswers(r Reader) []string {
	ctx := context.Background()
	_, e1 := r.Events(ctx, EventQuery{})
	_, e2 := r.Count(ctx, EventQuery{}, 10)
	_, e3 := r.Summary(ctx, SummaryQuery{})
	_, e4 := r.TimeSeries(ctx, TimeSeriesQuery{})
	_, e5 := r.Facet(ctx, FacetQuery{})
	var out []string
	for _, err := range []error{e1, e2, e3, e4, e5} {
		if err != nil {
			out = append(out, err.Error())
		}
	}
	return out
}

func TestController_Reconcile(t *testing.T) {
	src := usageSettings("file")
	var b backends
	c := NewController(src, b.build, EmitterOptions{}, testLogger())
	defer c.Close()
	ctx := context.Background()

	wantApplied := func(step, backend string, has bool, built ...string) {
		t.Helper()
		got, ok := c.Applied()
		if got.Backend != backend || ok != has || !slices.Equal(b.names(), built) {
			t.Fatalf("%s: applied %q (%v), built %v; want %q (%v), built %v", step, got.Backend, ok, b.names(), backend, has, built)
		}
	}
	wantApplied("before the first reconcile", "", false)
	if got := readAnswers(c.Reader()); got != nil {
		t.Fatalf("reads with no backend = %v, want empty answers", got)
	}

	c.reconcile(ctx)
	wantApplied("first reconcile", "file", true, "file")
	c.reconcile(ctx)
	wantApplied("same config", "file", true, "file")

	// A failed build keeps the working backend, and is retried on the next pass.
	src.Set(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "bad"})
	c.reconcile(ctx)
	c.reconcile(ctx)
	wantApplied("failed build", "file", true, "file")
	if _, closed := b.built[0].state(); closed != 0 {
		t.Fatal("a failed rebuild tore down the working sink")
	}

	src.Set(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "clickhouse"})
	c.reconcile(ctx)
	wantApplied("reroute", "clickhouse", true, "file", "clickhouse")
	if _, closed := b.built[0].state(); closed != 1 {
		t.Fatalf("old sink closed %d times on the swap, want 1", closed)
	}
	if _, closed := b.built[1].state(); closed != 0 {
		t.Fatal("the new sink was closed")
	}
	want := []string{"clickhouse.Events", "clickhouse.Count", "clickhouse.Summary", "clickhouse.TimeSeries", "clickhouse.Facet"}
	if got := readAnswers(c.Reader()); !slices.Equal(got, want) {
		t.Fatalf("reads = %v, want every method delegated to the live backend", got)
	}

	// A section that is absent or of another type is the zero config.
	for _, v := range []any{nil, "not a section", (*settings.UsageLogging)(nil)} {
		src.Set(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "valkey"})
		c.reconcile(ctx)
		if v == nil {
			src.Delete(settings.SectionUsageLogging)
		} else {
			src.Set(settings.SectionUsageLogging, v)
		}
		c.reconcile(ctx)
		if got, _ := c.Applied(); got != (settings.UsageLogging{}) {
			t.Fatalf("section %#v applied %+v, want the zero config", v, got)
		}
	}
}

// Run applies the stored section, then follows each change: events emitted
// after a reroute land in the new backend, and Close closes the live one.
func TestController_RunFollowsSettings(t *testing.T) {
	src := usageSettings("file")
	var b backends
	c := NewController(src, b.build, EmitterOptions{}, nil)
	c.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	waitApplied := func(backend string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if got, ok := c.Applied(); ok && got.Backend == backend {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("backend %q was never applied", backend)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitWritten := func(be *fakeBackend, want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			written, _ := be.state()
			if slices.Equal(written, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s wrote %v, want %v", be.name, written, want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	waitApplied("file")
	c.Emitter().Emit(served("to-file"))
	waitWritten(b.built[0], "to-file")

	src.Change(settings.SectionUsageLogging, &settings.UsageLogging{Backend: "postgres"})
	waitApplied("postgres")
	c.Emitter().Emit(served("to-postgres"))
	waitWritten(b.built[1], "to-postgres")

	cancel()
	<-done
	c.Close()
	if written, closed := b.built[0].state(); !slices.Equal(written, []string{"to-file"}) || closed != 1 {
		t.Fatalf("old backend: written %v, closed %d", written, closed)
	}
	if _, closed := b.built[1].state(); closed != 1 {
		t.Fatalf("live backend closed %d times by Close, want 1", closed)
	}
}

type failingSink struct{ calls int }

func (s *failingSink) Write(ev Event) error {
	s.calls++
	if s.calls%2 == 0 {
		return errors.New("refused " + ev.RequestID)
	}
	return nil
}

func TestReloadableSink(t *testing.T) {
	batch := []Event{{RequestID: "a"}, {RequestID: "b"}, {RequestID: "c"}, {RequestID: "d"}}

	t.Run("delegates to a batch sink", func(t *testing.T) {
		inner := openSink()
		r := &reloadableSink{cur: inner}
		if err := r.WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
		if err := r.Write(Event{RequestID: "e"}); err != nil {
			t.Fatal(err)
		}
		if sizes, written := inner.result(); !slices.Equal(sizes, []int{4, 1}) || !slices.Equal(written, []string{"a", "b", "c", "d", "e"}) {
			t.Fatalf("inner sink got batches %v, events %v", sizes, written)
		}
	})

	t.Run("loops Write for a sink without batches", func(t *testing.T) {
		inner := openSink()
		if err := (&reloadableSink{cur: writeOnly{inner}}).WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
		if sizes, written := inner.result(); !slices.Equal(sizes, []int{1, 1, 1, 1}) || !slices.Equal(written, []string{"a", "b", "c", "d"}) {
			t.Fatalf("inner sink got calls %v, events %v", sizes, written)
		}
	})

	t.Run("loops Write and reports the failures", func(t *testing.T) {
		inner := &failingSink{}
		err := (&reloadableSink{cur: inner}).WriteBatch(batch)
		if inner.calls != 4 {
			t.Fatalf("Write calls = %d, want every event attempted", inner.calls)
		}
		if err == nil || err.Error() != "2 of 4 events failed: refused b" {
			t.Fatalf("err = %v, want the count and the first failure", err)
		}
	})

	t.Run("no sink installed", func(t *testing.T) {
		r := &reloadableSink{}
		if err := errors.Join(r.WriteBatch(batch), r.Write(batch[0]), r.Close()); err != nil {
			t.Fatal(err)
		}
	})
}
