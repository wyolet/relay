package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/lifecycle"
)

func newLC(id string) *lifecycle.Context {
	return lifecycle.NewContext(id, "pipeline", time.Now())
}

// admit is a test shim for the PreFlight middleware signature.
func admit(a *Admission, lc *lifecycle.Context) error {
	return a.PreFlight(context.Background(), lc, &lifecycle.PreFlightEvent{})
}

func TestAdmission_DefaultCap(t *testing.T) {
	if got := NewAdmission(0).Cap(); got != DefaultMaxInflight {
		t.Fatalf("NewAdmission(0).Cap() = %d, want %d", got, DefaultMaxInflight)
	}
	if got := NewAdmission(-5).Cap(); got != DefaultMaxInflight {
		t.Fatalf("NewAdmission(-5).Cap() = %d, want %d", got, DefaultMaxInflight)
	}
	if got := NewAdmission(7).Cap(); got != 7 {
		t.Fatalf("NewAdmission(7).Cap() = %d, want 7", got)
	}
}

func TestAdmission_AcquireShedRelease(t *testing.T) {
	a := NewAdmission(2)

	lc1, lc2, lc3 := newLC("1"), newLC("2"), newLC("3")
	if err := admit(a, lc1); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := admit(a, lc2); err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	before, beforeEarly := testutil.ToFloat64(shedLateTotal), testutil.ToFloat64(shedEarlyTotal)
	if err := admit(a, lc3); !errors.Is(err, ErrShed) {
		t.Fatalf("third acquire err = %v, want ErrShed", err)
	}
	if after := testutil.ToFloat64(shedLateTotal); after != before+1 {
		t.Fatalf("late shed counter = %v, want %v", after, before+1)
	}
	if after := testutil.ToFloat64(shedEarlyTotal); after != beforeEarly {
		t.Fatalf("early shed counter moved on a PreFlight shed: %v, want %v", after, beforeEarly)
	}

	// Only the two admitted requests carry the release mark.
	if _, ok := lc1.Metadata[admittedKey]; !ok {
		t.Fatal("lc1 missing admit mark")
	}
	if _, ok := lc3.Metadata[admittedKey]; ok {
		t.Fatal("shed lc3 must not carry the admit mark")
	}

	// Releasing one admitted request frees exactly one slot.
	a.Collect(lc1)
	lc4 := newLC("4")
	if err := admit(a, lc4); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	// Pool full again.
	if err := admit(a, newLC("5")); !errors.Is(err, ErrShed) {
		t.Fatalf("acquire while full err = %v, want ErrShed", err)
	}
}

// TestAdmission_SlotHeldUntilCollect models the streaming case: the slot is
// held across the whole response and released only by Collect (which the
// runner fires at response-body close), never before.
func TestAdmission_SlotHeldUntilCollect(t *testing.T) {
	a := NewAdmission(1)

	streamLC := newLC("stream")
	if err := admit(a, streamLC); err != nil {
		t.Fatalf("stream acquire: %v", err)
	}
	// While the stream is open, a concurrent request is shed.
	if err := admit(a, newLC("other")); !errors.Is(err, ErrShed) {
		t.Fatalf("expected shed while slot held, got %v", err)
	}
	// Response body closes → Finalize → Collect releases the slot.
	a.Collect(streamLC)
	if err := admit(a, newLC("after-close")); err != nil {
		t.Fatalf("acquire after stream close: %v", err)
	}
}

// TestAdmission_ShedCollectNoOp proves a shed request (which never acquired)
// can't release a slot it didn't hold, so the semaphore can't drain negative.
func TestAdmission_ShedCollectNoOp(t *testing.T) {
	a := NewAdmission(1)

	held := newLC("held")
	if err := admit(a, held); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	shed := newLC("shed")
	if err := admit(a, shed); !errors.Is(err, ErrShed) {
		t.Fatalf("expected shed, got %v", err)
	}
	// Finalize runs Collect for the shed request too — it must be a no-op.
	a.Collect(shed)
	if err := admit(a, newLC("still-full")); !errors.Is(err, ErrShed) {
		t.Fatalf("slot must still be held after shed Collect, got %v", err)
	}
	// The genuine holder's release is the only one that frees the slot.
	a.Collect(held)
	if err := admit(a, newLC("now-free")); err != nil {
		t.Fatalf("acquire after real release: %v", err)
	}
}

// Full tracks the slots PreFlight and Collect take and return, and checking it takes none.
func TestAdmission_FullTracksSlots(t *testing.T) {
	var none *Admission
	if none.Full() {
		t.Fatal("nil Admission reports full")
	}

	a := NewAdmission(2)
	held := []*lifecycle.Context{newLC("1"), newLC("2")}
	for i, lc := range held {
		if a.Full() {
			t.Fatalf("full with %d of 2 slots held", i)
		}
		if err := admit(a, lc); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	if !a.Full() {
		t.Fatal("not full with every slot held")
	}
	a.Collect(held[0])
	if a.Full() {
		t.Fatal("still full after a release")
	}
	if err := admit(a, newLC("3")); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := admit(a, newLC("4")); !errors.Is(err, ErrShed) {
		t.Fatalf("acquire past the cap err = %v, want ErrShed", err)
	}
}

// RecordShed counts per stage and logs at most one line per interval, carrying every shed since the previous line.
func TestRecordShed_CountsAndSamplesLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	// Open a fresh interval with nothing pending from other tests.
	shedLastLogNanos.Store(0)
	shedEarlySinceLog.Store(0)
	shedLateSinceLog.Store(0)
	shedKVPoolSinceLog.Store(0)

	early, late, kvPool := testutil.ToFloat64(shedEarlyTotal), testutil.ToFloat64(shedLateTotal), testutil.ToFloat64(shedKVPoolTotal)
	RecordShed(ShedStageEarly) // first shed of the interval logs
	for range 3 {
		RecordShed(ShedStageEarly)
	}
	RecordShed(ShedStageLate)
	RecordShed(ShedStageKVPool)
	RecordShed(ShedStageKVPool)

	if got := testutil.ToFloat64(shedEarlyTotal) - early; got != 4 {
		t.Errorf("early shed counter rose by %v, want 4", got)
	}
	if got := testutil.ToFloat64(shedLateTotal) - late; got != 1 {
		t.Errorf("late shed counter rose by %v, want 1", got)
	}
	if got := testutil.ToFloat64(shedKVPoolTotal) - kvPool; got != 2 {
		t.Errorf("kv_pool shed counter rose by %v, want 2", got)
	}
	if n := strings.Count(buf.String(), "shedding load"); n != 1 {
		t.Fatalf("logged %d shed lines within one interval, want 1:\n%s", n, buf.String())
	}

	// The next interval's line carries the sheds held back since the first.
	buf.Reset()
	shedLastLogNanos.Store(time.Now().Add(-2 * shedLogInterval).UnixNano())
	RecordShed(ShedStageLate)
	var line struct {
		Shed, Early, Late int64
		KVPool            int64 `json:"kv_pool"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("shed line: %v (%q)", err, buf.String())
	}
	if line.Shed != 7 || line.Early != 3 || line.Late != 2 || line.KVPool != 2 {
		t.Fatalf("shed line = %+v, want shed 7 early 3 late 2 kv_pool 2", line)
	}
}

// TestAdmission_NilMetadataAdmitsWithoutHolding: a Context that can't carry the
// release mark is admitted rather than held — never take a slot we can't return.
func TestAdmission_NilMetadataAdmitsWithoutHolding(t *testing.T) {
	a := NewAdmission(1)

	noMeta := &lifecycle.Context{} // Metadata is nil
	if err := admit(a, noMeta); err != nil {
		t.Fatalf("nil-metadata admit: %v", err)
	}
	// It consumed no slot, so a real request still fits.
	if err := admit(a, newLC("real")); err != nil {
		t.Fatalf("real acquire should still fit: %v", err)
	}
	if err := admit(a, newLC("overflow")); !errors.Is(err, ErrShed) {
		t.Fatalf("expected shed once the real request holds the slot, got %v", err)
	}
	// nil-safe collector.
	a.Collect(nil)
	a.Collect(noMeta)
}
