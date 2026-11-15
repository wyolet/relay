package meta

import (
	"errors"
	"testing"
)

func TestExpectedVersion(t *testing.T) {
	if _, ok, err := (Metadata{}).ExpectedVersion(); ok || err != nil {
		t.Fatalf("unset version: ok=%v err=%v, want an unconditional write", ok, err)
	}
	v, ok, err := Metadata{ResourceVersion: FormatResourceVersion(42)}.ExpectedVersion()
	if !ok || err != nil || v != 42 {
		t.Fatalf("round trip: v=%d ok=%v err=%v", v, ok, err)
	}
	if _, _, err := (Metadata{ResourceVersion: "W/abc"}).ExpectedVersion(); !errors.Is(err, ErrStaleResourceVersion) {
		t.Fatalf("unparseable version err = %v, want ErrStaleResourceVersion", err)
	}
}

func TestStaleIfNoRows(t *testing.T) {
	if err := StaleIfNoRows(1, nil); err != nil {
		t.Fatalf("one row: %v", err)
	}
	if err := StaleIfNoRows(0, nil); !errors.Is(err, ErrStaleResourceVersion) {
		t.Fatalf("no rows: %v", err)
	}
	boom := errors.New("boom")
	if err := StaleIfNoRows(0, boom); !errors.Is(err, boom) {
		t.Fatalf("store error not passed through: %v", err)
	}
}
