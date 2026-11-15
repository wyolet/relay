package valkey_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/usage"
	"github.com/wyolet/relay/pkg/usage/valkey"
)

// Query semantics are pinned by the conformance suite; the tests here cover what is this backend's own: how events are keyed, expired and scanned.

func newSink(t *testing.T) *valkey.Sink {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	return valkey.New(mem, valkey.Config{TTL: time.Minute})
}

// recordingStore records Set calls and serves Range from them, or fails it.
type recordingStore struct {
	entries  []kv.Entry
	ttls     []time.Duration
	prefixes []string
	rangeErr error
}

func (s *recordingStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.entries = append(s.entries, kv.Entry{Key: key, Value: value})
	s.ttls = append(s.ttls, ttl)
	return nil
}

func (s *recordingStore) Range(_ context.Context, prefix string) ([]kv.Entry, error) {
	s.prefixes = append(s.prefixes, prefix)
	return s.entries, s.rangeErr
}

func TestWriteKeysEventsByTimestampUnderOneHashTag(t *testing.T) {
	ts := time.Date(2026, 1, 5, 10, 0, 0, 123, time.UTC)
	for _, tc := range []struct {
		name    string
		cfg     valkey.Config
		wantKey string
		wantTTL time.Duration
	}{
		{"defaults", valkey.Config{}, "usagevk:{u}:01767607200000000123:req-1", 24 * time.Hour},
		{"configured", valkey.Config{TTL: time.Minute, KeyPrefix: "other"}, "other:{u}:01767607200000000123:req-1", time.Minute},
		{"a negative ttl falls back to the default", valkey.Config{TTL: -time.Second}, "usagevk:{u}:01767607200000000123:req-1", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingStore{}
			want := usage.Event{RequestID: "req-1", Timestamp: ts, Status: 200, Source: "pipeline", Model: "m",
				CostNanos: new(int64), CostBreakdown: map[string]int64{"tokens.input": 7}}
			sk := valkey.New(store, tc.cfg)
			if err := sk.Write(want); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if len(store.entries) != 1 || store.entries[0].Key != tc.wantKey || store.ttls[0] != tc.wantTTL {
				t.Fatalf("stored %d entries, key %q ttl %v; want key %q ttl %v", len(store.entries), store.entries[0].Key, store.ttls[0], tc.wantKey, tc.wantTTL)
			}
			got, err := sk.Events(context.Background(), usage.EventQuery{})
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if diff := cmp.Diff([]usage.Event{want}, got); diff != "" {
				t.Errorf("event read back (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{tc.wantKey[:len(tc.wantKey)-len("01767607200000000123:req-1")]}, store.prefixes); diff != "" {
				t.Errorf("range prefix (-want +got):\n%s", diff)
			}
		})
	}
}

// An event without a timestamp is keyed at the write instant, and still stored as written.
func TestWriteKeysAZeroTimestampAtNow(t *testing.T) {
	store := &recordingStore{}
	before := time.Now().UnixNano()
	if err := valkey.New(store, valkey.Config{}).Write(usage.Event{RequestID: "r"}); err != nil {
		t.Fatal(err)
	}
	var nanos int64
	for _, c := range store.entries[0].Key[len("usagevk:{u}:") : len("usagevk:{u}:")+20] {
		nanos = nanos*10 + int64(c-'0')
	}
	if nanos < before || nanos > time.Now().UnixNano() {
		t.Errorf("key %q is not stamped between the write's start and end", store.entries[0].Key)
	}
}

func TestMalformedValueSkipped(t *testing.T) {
	store := &recordingStore{entries: []kv.Entry{
		{Key: "usagevk:{u}:00000000000000000001:bad", Value: []byte("not-json")},
		{Key: "usagevk:{u}:00000000000000000002:good", Value: []byte(`{"request_id":"good","status":200}`)},
	}}
	sk := valkey.New(store, valkey.Config{})
	got, err := sk.Events(context.Background(), usage.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]usage.Event{{RequestID: "good", Status: 200}}, got); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if n, err := sk.Count(context.Background(), usage.EventQuery{}, 0); err != nil || n != 1 {
		t.Errorf("Count = %d, %v; want 1", n, err)
	}
}

func TestStoreErrorFailsEveryRead(t *testing.T) {
	down := errors.New("store down")
	sk := valkey.New(&recordingStore{rangeErr: down}, valkey.Config{})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Events":     func() error { _, err := sk.Events(ctx, usage.EventQuery{}); return err },
		"Count":      func() error { _, err := sk.Count(ctx, usage.EventQuery{}, 0); return err },
		"Summary":    func() error { _, err := sk.Summary(ctx, usage.SummaryQuery{}); return err },
		"TimeSeries": func() error { _, err := sk.TimeSeries(ctx, usage.TimeSeriesQuery{Interval: time.Hour}); return err },
		"Facet":      func() error { _, err := sk.Facet(ctx, usage.FacetQuery{Dim: "model"}); return err },
	} {
		if err := call(); !errors.Is(err, down) {
			t.Errorf("%s: err = %v, want the store's", name, err)
		}
	}
}

func TestInvalidQueriesAreRejected(t *testing.T) {
	sk := newSink(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		call func() error
		want string
	}{
		"summary group_by":    {func() error { _, err := sk.Summary(ctx, usage.SummaryQuery{GroupBy: "bogus"}); return err }, `usage.Summarize: invalid group_by "bogus"`},
		"timeseries interval": {func() error { _, err := sk.TimeSeries(ctx, usage.TimeSeriesQuery{}); return err }, "usage.Bucketize: interval must be > 0"},
		"facet dim":           {func() error { _, err := sk.Facet(ctx, usage.FacetQuery{Dim: "bogus"}); return err }, `usage.TopValues: invalid dim "bogus"`},
	} {
		if err := tc.call(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
