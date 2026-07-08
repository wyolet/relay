package httpapi

import (
	"context"
	"errors"
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

	before := testutil.ToFloat64(requestsShedTotal)
	if err := admit(a, lc3); !errors.Is(err, ErrShed) {
		t.Fatalf("third acquire err = %v, want ErrShed", err)
	}
	if after := testutil.ToFloat64(requestsShedTotal); after != before+1 {
		t.Fatalf("shed counter = %v, want %v", after, before+1)
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
