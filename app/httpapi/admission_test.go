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

func TestNewAdmission_Cap(t *testing.T) {
	for max, want := range map[int]int{0: DefaultMaxInflight, -5: DefaultMaxInflight, 1: 1, 7: 7} {
		if a := NewAdmission(max); a.Cap() != want || a.Limit() != want {
			t.Errorf("NewAdmission(%d): cap %d limit %d, want both %d", max, a.Cap(), a.Limit(), want)
		}
	}
}

// A slot is taken by PreFlight, held across the whole response, and returned only by its holder's Collect: a streamed request frees it at body close, and a shed request, which holds none, frees none. Full follows the slots and takes none itself.
func TestAdmission_SlotLifecycle(t *testing.T) {
	var none *Admission
	if none.Full() {
		t.Fatal("nil Admission reports full")
	}
	a := NewAdmission(2)
	held1, held2, shed := newLC("1"), newLC("2"), newLC("3")
	late, early := testutil.ToFloat64(shedLateTotal), testutil.ToFloat64(shedEarlyTotal)

	step := func(what string, err, want error, slots int) {
		t.Helper()
		if !errors.Is(err, want) || len(a.sem) != slots || a.Full() != (slots == 2) {
			t.Fatalf("%s: err %v with %d slots held and full=%v; want %v with %d", what, err, len(a.sem), a.Full(), want, slots)
		}
	}
	step("first admit", admit(a, held1), nil, 1)
	step("second admit", admit(a, held2), nil, 2)
	step("admit past the cap", admit(a, shed), ErrShed, 2)
	if _, ok := shed.Metadata[admittedKey]; ok {
		t.Fatal("the shed request carries the admit mark")
	}
	if _, ok := held1.Metadata[admittedKey]; !ok {
		t.Fatal("an admitted request lacks the admit mark")
	}
	if got, gotEarly := testutil.ToFloat64(shedLateTotal)-late, testutil.ToFloat64(shedEarlyTotal)-early; got != 1 || gotEarly != 0 {
		t.Fatalf("a PreFlight shed counted late %v early %v, want 1 and 0", got, gotEarly)
	}

	a.Collect(shed)
	step("Collect of the shed request", nil, nil, 2)
	a.Collect(nil)
	step("Collect of nil", nil, nil, 2)
	a.Collect(held1)
	step("Collect of a holder", nil, nil, 1)
	step("admit into the freed slot", admit(a, newLC("4")), nil, 2)
	step("admit while full again", admit(a, newLC("5")), ErrShed, 2)
}

// A Context that cannot carry the release mark is admitted without a slot: never take one that cannot be returned.
func TestAdmission_NilMetadataAdmitsWithoutHolding(t *testing.T) {
	a := NewAdmission(1)
	noMeta := &lifecycle.Context{}
	for _, lc := range []*lifecycle.Context{noMeta, nil} {
		if err := admit(a, lc); err != nil || len(a.sem) != 0 {
			t.Fatalf("admit of %v = %v with %d slots held, want admitted holding none", lc, err, len(a.sem))
		}
	}
	if err := admit(a, newLC("real")); err != nil {
		t.Fatalf("a real request should still fit: %v", err)
	}
	// Even at the cap such a request is let through, and its Collect releases nothing.
	if err := admit(a, noMeta); err != nil {
		t.Fatalf("admit of a markless context at the cap = %v", err)
	}
	a.Collect(noMeta)
	if len(a.sem) != 1 {
		t.Fatalf("slots held = %d, want the real request's 1", len(a.sem))
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
	// A stage nothing names counts as late.
	RecordShed("somewhere")

	if e, l, k := testutil.ToFloat64(shedEarlyTotal)-early, testutil.ToFloat64(shedLateTotal)-late, testutil.ToFloat64(shedKVPoolTotal)-kvPool; e != 4 || l != 2 || k != 2 {
		t.Errorf("counters rose by early %v late %v kv_pool %v, want 4, 2, 2", e, l, k)
	}
	type shedLine struct {
		Msg               string
		Shed, Early, Late int64
		KVPool            int64 `json:"kv_pool"`
	}
	var first, next shedLine
	if n := strings.Count(buf.String(), "\n"); n != 1 || json.Unmarshal(buf.Bytes(), &first) != nil || first != (shedLine{"inference: shedding load", 1, 1, 0, 0}) {
		t.Fatalf("logged %d lines within one interval, want the first shed alone:\n%s", n, buf.String())
	}

	// The next interval's line carries the sheds held back since the first.
	buf.Reset()
	shedLastLogNanos.Store(time.Now().Add(-2 * shedLogInterval).UnixNano())
	RecordShed(ShedStageLate)
	if err := json.Unmarshal(buf.Bytes(), &next); err != nil || next != (shedLine{"inference: shedding load", 8, 3, 3, 2}) {
		t.Fatalf("next line = %+v (%v), want shed 8 early 3 late 3 kv_pool 2: %q", next, err, buf.String())
	}
}
