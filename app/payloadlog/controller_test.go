package payloadlog

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
)

// sinks builds one recording sink per config, keyed by its S3 bucket; a "bad" bucket fails to build.
type sinks struct {
	mu    sync.Mutex
	built []string
	by    map[string]*batchSink
}

func (s *sinks) build(_ context.Context, cfg settings.PayloadLogging) (Sink, error) {
	if cfg.S3.Bucket == "bad" {
		return nil, io.ErrUnexpectedEOF
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]*batchSink{}
	}
	sink := openSink()
	s.built = append(s.built, cfg.S3.Bucket)
	s.by[cfg.S3.Bucket] = sink
	return sink, nil
}

func (s *sinks) closes(bucket string) int {
	s.mu.Lock()
	sink := s.by[bucket]
	s.mu.Unlock()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.closed
}

func bucket(name string, maxBytes int) *settings.PayloadLogging {
	return &settings.PayloadLogging{Enabled: true, Backend: "s3", MaxBytes: maxBytes, S3: settings.PayloadS3{Bucket: name}}
}

func TestController_Reconcile(t *testing.T) {
	src := &settingstest.Source{}
	var s sinks
	c := NewController(src, s.build, EmitterOptions{}, testLogger())
	defer c.Close()
	if _, has := c.Applied(); has || c.Enabled() {
		t.Fatal("a fresh controller reports an applied section")
	}

	type state struct {
		enabled  bool
		maxBytes int
		built    []string
	}
	steps := []struct {
		name    string
		section any // nil deletes the section
		want    state
		applied settings.PayloadLogging
	}{
		{"no section", nil, state{false, 0, nil}, settings.PayloadLogging{}},
		{"enable A", bucket("A", 7), state{true, 7, []string{"A"}}, *bucket("A", 7)},
		{"same section", bucket("A", 7), state{true, 7, []string{"A"}}, *bucket("A", 7)},
		{"swap to B", bucket("B", 9), state{true, 9, []string{"A", "B"}}, *bucket("B", 9)},
		{"failed build keeps B", bucket("bad", 99), state{true, 9, []string{"A", "B"}}, *bucket("B", 9)},
		{"disable keeps the cap it was given", &settings.PayloadLogging{MaxBytes: 3}, state{false, 3, []string{"A", "B"}}, settings.PayloadLogging{MaxBytes: 3}},
		{"section of another type", "not a section", state{false, 0, []string{"A", "B"}}, settings.PayloadLogging{}},
		{"nil section", (*settings.PayloadLogging)(nil), state{false, 0, []string{"A", "B"}}, settings.PayloadLogging{}},
	}
	for _, step := range steps {
		if step.section == nil {
			src.Delete(settings.SectionPayloadLogging)
		} else {
			src.Set(settings.SectionPayloadLogging, step.section)
		}
		c.reconcile(context.Background())
		got := state{c.Enabled(), c.MaxBytes(), slices.Clone(s.built)}
		applied, has := c.Applied()
		if got.enabled != step.want.enabled || got.maxBytes != step.want.maxBytes || !slices.Equal(got.built, step.want.built) || applied != step.applied || !has {
			t.Fatalf("%s: state %+v, applied %+v (%v); want %+v, applied %+v", step.name, got, applied, has, step.want, step.applied)
		}
		switch step.name {
		case "swap to B":
			if s.closes("A") != 1 || s.closes("B") != 0 {
				t.Fatalf("after the swap: A closed %d, B closed %d; want the old sink closed once", s.closes("A"), s.closes("B"))
			}
		case "failed build keeps B":
			if s.closes("B") != 0 {
				t.Fatal("a failed rebuild tore down the working sink")
			}
		case "disable keeps the cap it was given":
			if s.closes("B") != 1 {
				t.Fatalf("after disabling: B closed %d times, want 1", s.closes("B"))
			}
		}
	}
}

// Run applies the stored section, then follows each change; records emitted
// while logging is on land in the sink of the moment.
func TestController_RunFollowsSettings(t *testing.T) {
	src := &settingstest.Source{}
	var s sinks
	c := NewController(src, s.build, EmitterOptions{}, nil)
	c.Subscribe()
	if n := src.Callbacks(settings.SectionPayloadLogging); n != 1 {
		t.Fatalf("Subscribe registered %d callbacks, want 1", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	wrote := func(name string, want ...string) func() bool {
		return func() bool {
			s.mu.Lock()
			sink := s.by[name]
			s.mu.Unlock()
			_, written := sink.result()
			return slices.Equal(written, want)
		}
	}

	src.Change(settings.SectionPayloadLogging, bucket("A", 5))
	wait("A to be applied", func() bool { return c.Enabled() && c.MaxBytes() == 5 })
	c.Emitter().Emit(rec("to-A"))
	wait("the record to reach A", wrote("A", "to-A"))

	src.Change(settings.SectionPayloadLogging, bucket("B", 6))
	wait("B to be applied", func() bool { return c.MaxBytes() == 6 })
	c.Emitter().Emit(rec("to-B"))
	wait("the record to reach B", wrote("B", "to-B"))

	cancel()
	<-done
	c.Close()
	if !wrote("A", "to-A")() || s.closes("A") != 1 || s.closes("B") != 1 {
		t.Fatalf("after Close: A closed %d, B closed %d; want each closed once and A untouched since the swap", s.closes("A"), s.closes("B"))
	}
}

type failingSink struct{ calls int }

func (s *failingSink) Write(r Record) error {
	s.calls++
	if s.calls%2 == 0 {
		return errors.New("refused " + r.RequestID)
	}
	return nil
}

func TestReloadableSink(t *testing.T) {
	batch := []Record{rec("a"), rec("b"), rec("c"), rec("d")}

	t.Run("delegates to a batch sink", func(t *testing.T) {
		inner := openSink()
		r := &reloadableSink{cur: inner}
		if err := errors.Join(r.WriteBatch(batch), r.Write(rec("e"))); err != nil {
			t.Fatal(err)
		}
		if sizes, written := inner.result(); !slices.Equal(sizes, []int{4, 1}) || !slices.Equal(written, []string{"a", "b", "c", "d", "e"}) {
			t.Fatalf("inner sink got batches %v, records %v", sizes, written)
		}
	})

	t.Run("loops Write for a sink without batches", func(t *testing.T) {
		inner := openSink()
		if err := (&reloadableSink{cur: writeOnly{inner}}).WriteBatch(batch); err != nil {
			t.Fatal(err)
		}
		if sizes, written := inner.result(); !slices.Equal(sizes, []int{1, 1, 1, 1}) || !slices.Equal(written, []string{"a", "b", "c", "d"}) {
			t.Fatalf("inner sink got calls %v, records %v", sizes, written)
		}
	})

	t.Run("loops Write and reports the failures", func(t *testing.T) {
		inner := &failingSink{}
		err := (&reloadableSink{cur: inner}).WriteBatch(batch)
		if inner.calls != 4 {
			t.Fatalf("Write calls = %d, want every record attempted", inner.calls)
		}
		if err == nil || err.Error() != "2 of 4 records failed: refused b" {
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
