package catalog

import (
	"testing"
)

// ── parseEvent ────────────────────────────────────────────────────────────────

func TestParseEvent_HappyPath(t *testing.T) {
	kinds := []string{"provider", "host", "model", "hostkey", "ratelimit", "policy", "pricing", "relaykey"}
	ops := []string{"upsert", "delete"}
	for _, kind := range kinds {
		for _, op := range ops {
			payload := kind + ":" + op + ":some-uuid-1234"
			ev, ok := parseEvent(payload)
			if !ok {
				t.Errorf("parseEvent(%q) returned false, want true", payload)
				continue
			}
			if ev.Kind != kind || ev.Op != op || ev.ID != "some-uuid-1234" {
				t.Errorf("parseEvent(%q) = %+v, unexpected fields", payload, ev)
			}
		}
	}
}

func TestParseEvent_Malformed(t *testing.T) {
	cases := []string{
		"",
		"provider:upsert",             // too few segments
		"provider:upsert:id:extra",    // too many segments (4 parts after SplitN(4))
		"badkind:upsert:id",           // unknown kind
		"provider:badop:id",           // unknown op
		"provider:upsert:",            // empty id
	}
	for _, payload := range cases {
		_, ok := parseEvent(payload)
		if ok {
			t.Errorf("parseEvent(%q) returned true, want false", payload)
		}
	}
}

// ── debouncer ─────────────────────────────────────────────────────────────────

func TestDebouncer_Coalesces(t *testing.T) {
	d := newDebouncer(0)
	ev := notifyEvent{Kind: "model", Op: "upsert", ID: "X"}
	d.push(ev)
	d.push(ev)
	d.push(ev)
	drained := d.drain()
	if len(drained) != 1 {
		t.Fatalf("drain() returned %d events, want 1", len(drained))
	}
}

func TestDebouncer_LastOpWins(t *testing.T) {
	d := newDebouncer(0)
	d.push(notifyEvent{Kind: "model", Op: "upsert", ID: "X"})
	d.push(notifyEvent{Kind: "model", Op: "delete", ID: "X"})
	drained := d.drain()
	if len(drained) != 1 {
		t.Fatalf("drain() returned %d events, want 1", len(drained))
	}
	if drained[0].Op != "delete" {
		t.Errorf("op = %q, want delete", drained[0].Op)
	}
}

func TestDebouncer_DifferentRowsKept(t *testing.T) {
	d := newDebouncer(0)
	d.push(notifyEvent{Kind: "model", Op: "upsert", ID: "X"})
	d.push(notifyEvent{Kind: "model", Op: "upsert", ID: "Y"})
	drained := d.drain()
	if len(drained) != 2 {
		t.Fatalf("drain() returned %d events, want 2", len(drained))
	}
}
