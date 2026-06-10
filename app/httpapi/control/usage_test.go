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

func TestParseTagFilters(t *testing.T) {
	tags, err := parseTagFilters([]string{"session_id:s1", "session_id:s2", "leg:translate", "colon:a:b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := tags["session_id"]; len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("session_id values: %+v", got)
	}
	if got := tags["leg"]; len(got) != 1 || got[0] != "translate" {
		t.Fatalf("leg values: %+v", got)
	}
	if got := tags["colon"]; len(got) != 1 || got[0] != "a:b" {
		t.Fatalf("value keeps extra colons: %+v", got)
	}

	if got, err := parseTagFilters(nil); err != nil || got != nil {
		t.Fatalf("empty input: %+v, %v", got, err)
	}
	for _, bad := range []string{"nocolon", ":valueonly"} {
		if _, err := parseTagFilters([]string{bad}); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
}
