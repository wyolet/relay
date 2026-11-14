package hosthealth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/kv"
)

// recordingStore counts Set calls and keeps the last TTL.
type recordingStore struct {
	kv.Store
	sets    int
	lastTTL time.Duration
}

func (s *recordingStore) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	s.sets++
	s.lastTTL = ttl
	return s.Store.Set(ctx, k, v, ttl)
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newRecorder() (*hosthealth.Recorder, *recordingStore, *time.Time) {
	st := &recordingStore{Store: kv.NewMem()}
	now := t0
	return hosthealth.New(st, func() time.Time { return now }), st, &now
}

func wantStatus(t *testing.T, r *hosthealth.Recorder, hostID string, want host.Status) {
	t.Helper()
	got, found := r.Read(context.Background(), hostID)
	if !found {
		t.Fatalf("Read(%q): no record, want %+v", hostID, want)
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("Read(%q) (-want +got):\n%s", hostID, d)
	}
}

func TestRecorder_StateTransitions(t *testing.T) {
	r, st, now := newRecorder()
	ctx := context.Background()

	if got, found := r.Read(ctx, "h1"); found || got != (host.Status{}) {
		t.Fatalf("before any observation: %+v found=%v, want zero/false", got, found)
	}

	r.Reachable(ctx, "h1")
	wantStatus(t, r, "h1", host.Status{Health: host.HealthHealthy, LastTransition: t0, LastSuccess: t0})
	if st.lastTTL != time.Hour {
		t.Errorf("record TTL = %v, want 1h", st.lastTTL)
	}

	t1 := t0.Add(time.Minute)
	*now = t1
	r.Unreachable(ctx, "h1", "dial tcp: refused")
	wantStatus(t, r, "h1", host.Status{
		Health: host.HealthUnreachable, LastError: "dial tcp: refused",
		ConsecutiveFailures: 1, LastTransition: t1, LastSuccess: t0,
	})

	t2 := t1.Add(time.Minute)
	*now = t2
	r.Unreachable(ctx, "h1", strings.Repeat("x", 300))
	wantStatus(t, r, "h1", host.Status{
		Health: host.HealthUnreachable, LastError: strings.Repeat("x", 256),
		ConsecutiveFailures: 2, LastTransition: t2, LastSuccess: t0,
	})

	// Recovery drops the error and the failure count.
	r.Reachable(ctx, "h1")
	wantStatus(t, r, "h1", host.Status{Health: host.HealthHealthy, LastTransition: t2, LastSuccess: t2})

	// A host that never answered has no LastSuccess.
	r.Unreachable(ctx, "h2", "no route")
	wantStatus(t, r, "h2", host.Status{
		Health: host.HealthUnreachable, LastError: "no route", ConsecutiveFailures: 1, LastTransition: t2,
	})
}

func TestRecorder_ReachableDebouncedPerHost(t *testing.T) {
	r, st, now := newRecorder()
	ctx := context.Background()
	step := func(name string, want int) {
		t.Helper()
		if st.sets != want {
			t.Fatalf("%s: sets = %d, want %d", name, st.sets, want)
		}
	}

	for range 100 {
		r.Reachable(ctx, "h1")
	}
	step("burst in one instant", 1)

	*now = t0.Add(time.Second - time.Nanosecond)
	r.Reachable(ctx, "h1")
	step("just inside the interval", 1)

	*now = t0.Add(time.Second)
	r.Reachable(ctx, "h1")
	r.Reachable(ctx, "h1")
	step("at the interval", 2)
	wantStatus(t, r, "h1", host.Status{Health: host.HealthHealthy, LastTransition: *now, LastSuccess: *now})

	r.Reachable(ctx, "h2")
	step("another host has its own gate", 3)

	// Failures are never debounced and reopen the gate for recovery.
	r.Unreachable(ctx, "h1", "refused")
	r.Unreachable(ctx, "h1", "refused")
	step("two failures in one instant", 5)
	r.Reachable(ctx, "h1")
	step("recovery inside the interval", 6)
	r.Reachable(ctx, "h1")
	step("gate closed again after recovery", 6)
}

func TestRecorder_IgnoresEmptyHostAndNilReceiver(t *testing.T) {
	ctx := context.Background()
	r, st, _ := newRecorder()
	r.Reachable(ctx, "")
	r.Unreachable(ctx, "", "boom")
	if st.sets != 0 {
		t.Errorf("empty host id wrote %d records, want 0", st.sets)
	}
	if _, found := r.Read(ctx, ""); found {
		t.Error("Read(\"\") found a record")
	}

	for name, rec := range map[string]*hosthealth.Recorder{"nil recorder": nil, "nil store": hosthealth.New(nil, nil)} {
		rec.Reachable(ctx, "h1")
		rec.Unreachable(ctx, "h1", "boom")
		if _, found := rec.Read(ctx, "h1"); found {
			t.Errorf("%s: Read found a record", name)
		}
		if all := rec.ReadAll(ctx); all != nil {
			t.Errorf("%s: ReadAll = %v, want nil", name, all)
		}
	}
}

func TestRecorder_ReadAllSkipsForeignAndUndecodableRecords(t *testing.T) {
	r, st, _ := newRecorder()
	ctx := context.Background()

	if all := r.ReadAll(ctx); all != nil {
		t.Fatalf("ReadAll on an empty store = %v, want nil", all)
	}

	r.Reachable(ctx, "h1")
	r.Unreachable(ctx, "h2", "refused")
	for k, v := range map[string]string{
		"host_health:{host:bad}": "not json",
		"host_health:{host:}":    `{"health":"healthy"}`,
		"host_health:other":      `{"health":"healthy"}`,
	} {
		if err := st.Store.Set(ctx, k, []byte(v), time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]host.Status{
		"h1": {Health: host.HealthHealthy, LastTransition: t0, LastSuccess: t0},
		"h2": {Health: host.HealthUnreachable, LastError: "refused", ConsecutiveFailures: 1, LastTransition: t0},
	}
	if d := cmp.Diff(want, r.ReadAll(ctx)); d != "" {
		t.Errorf("ReadAll (-want +got):\n%s", d)
	}
	if got, found := r.Read(ctx, "bad"); found || got != (host.Status{}) {
		t.Errorf("Read of an undecodable record = %+v found=%v, want zero/false", got, found)
	}
}
