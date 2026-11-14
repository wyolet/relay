package catalog

import (
	"cmp"
	"slices"
	"strconv"
	"testing"
)

// A payload is "kind:op:id" over the known kinds and the two ops; the id is everything after the second colon, colons included, as settings section keys are colon-namespaced.
func TestParseEvent(t *testing.T) {
	for kind := range validKinds {
		for _, op := range []string{"upsert", "delete"} {
			want := notifyEvent{Kind: kind, Op: op, ID: "some-uuid-1234"}
			if ev, ok := parseEvent(kind + ":" + op + ":some-uuid-1234"); !ok || ev != want {
				t.Errorf("parseEvent(%s:%s:some-uuid-1234) = %+v, %v; want %+v", kind, op, ev, ok, want)
			}
		}
	}
	want := notifyEvent{Kind: "settings", Op: "upsert", ID: "governance:policy"}
	if ev, ok := parseEvent("settings:upsert:governance:policy"); !ok || ev != want {
		t.Errorf("colon-namespaced id = %+v, %v; want %+v", ev, ok, want)
	}
	for name, payload := range map[string]string{
		"empty":        "",
		"no id":        "provider:upsert",
		"unknown kind": "badkind:upsert:id",
		"unknown op":   "provider:update:id",
		"empty id":     "provider:upsert:",
		"kind only":    "provider",
	} {
		if ev, ok := parseEvent(payload); ok || ev != (notifyEvent{}) {
			t.Errorf("%s: parseEvent(%q) = %+v, %v; want nothing", name, payload, ev, ok)
		}
	}
}

func TestDebouncer(t *testing.T) {
	up := func(id string) notifyEvent { return notifyEvent{Kind: "model", Op: "upsert", ID: id} }
	del := func(id string) notifyEvent { return notifyEvent{Kind: "model", Op: "delete", ID: id} }
	for _, tc := range []struct {
		name    string
		pushed  []notifyEvent
		requeue []drainedEvent
		want    []drainedEvent
	}{
		{name: "nothing pending"},
		{name: "repeats of one row coalesce", pushed: []notifyEvent{up("X"), up("X"), up("X")}, want: []drainedEvent{{"model", "X", "upsert"}}},
		{name: "the last op on a row wins", pushed: []notifyEvent{up("X"), del("X")}, want: []drainedEvent{{"model", "X", "delete"}}},
		{name: "different rows are kept", pushed: []notifyEvent{up("X"), up("Y")}, want: []drainedEvent{{"model", "X", "upsert"}, {"model", "Y", "upsert"}}},
		{name: "one id under two kinds is two rows", pushed: []notifyEvent{up("X"), {Kind: "host", Op: "delete", ID: "X"}}, want: []drainedEvent{{"host", "X", "delete"}, {"model", "X", "upsert"}}},
		{name: "a requeued event comes back", requeue: []drainedEvent{{"model", "X", "upsert"}}, want: []drainedEvent{{"model", "X", "upsert"}}},
		{name: "a newer event beats a requeued one", pushed: []notifyEvent{del("X")}, requeue: []drainedEvent{{"model", "X", "upsert"}}, want: []drainedEvent{{"model", "X", "delete"}}},
	} {
		d := newDebouncer(0)
		for _, e := range tc.pushed {
			if d.push(e) {
				t.Errorf("%s: push asked for a flush below the cap", tc.name)
			}
		}
		for _, e := range tc.requeue {
			d.requeue(e)
		}
		got := d.drain()
		slices.SortFunc(got, func(a, b drainedEvent) int { return cmp.Compare(a.Kind+a.ID, b.Kind+b.ID) })
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: drained %v, want %v", tc.name, got, tc.want)
		}
		if again := d.drain(); again != nil {
			t.Errorf("%s: a second drain returned %v", tc.name, again)
		}
	}
}

// A buffer that reaches the cap asks its caller for an immediate flush.
func TestDebouncer_PushAsksForAFlushAtTheCap(t *testing.T) {
	d := newDebouncer(0)
	for i := range debounceCap - 1 {
		if d.push(notifyEvent{Kind: "model", Op: "upsert", ID: strconv.Itoa(i)}) {
			t.Fatalf("push %d asked for a flush below the cap of %d", i, debounceCap)
		}
	}
	if !d.push(notifyEvent{Kind: "model", Op: "upsert", ID: "last"}) {
		t.Fatal("the push reaching the cap asked for no flush")
	}
	if n := len(d.drain()); n != debounceCap {
		t.Fatalf("drained %d events, want %d", n, debounceCap)
	}
}
