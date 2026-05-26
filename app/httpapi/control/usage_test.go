package control

import (
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Now().UTC()
	id := "req_01HXYZ:weird-but-valid"

	tok := encodeCursor(ts, id)
	gotTS, gotID, err := decodeCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotTS.Equal(ts) {
		t.Fatalf("ts mismatch: got %v want %v", gotTS, ts)
	}
	if gotID != id {
		t.Fatalf("id mismatch: got %q want %q", gotID, id)
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	for _, bad := range []string{"not base64!!", "", "bm9jb2xvbg"} { // last decodes to "nocolon"
		if _, _, err := decodeCursor(bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}

func TestEffectiveLimit(t *testing.T) {
	if got := effectiveLimit(0); got != 100 {
		t.Fatalf("default: got %d", got)
	}
	if got := effectiveLimit(50); got != 50 {
		t.Fatalf("passthrough: got %d", got)
	}
	if got := effectiveLimit(1 << 30); got != 10000 {
		t.Fatalf("clamp: got %d", got)
	}
}
