package otlpreceiver_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/go-cmp/cmp"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/otlpreceiver"
	"github.com/wyolet/relay/pkg/kv"
)

// tenant is the reporter of every marker test that does not name another.
const tenant = "project-a"

func call(trace, span int) otlpreceiver.Call {
	return otlpreceiver.Call{TraceID: fmt.Sprintf("%032x", trace), SpanID: fmt.Sprintf("%016x", span)}
}

// runMarkersSuite is the backend-independent contract, run against kv.Mem here and against a real Redis in the integration build. Each subtest uses trace ids of its own, so they share one store.
func runMarkersSuite(t *testing.T, s kv.Scripter) {
	t.Helper()
	m := otlpreceiver.NewMarkers(s)
	markFor := func(t *testing.T, who string, kind otlpreceiver.MarkerKind, calls ...otlpreceiver.Call) []bool {
		t.Helper()
		fresh, err := m.Mark(t.Context(), who, kind, calls)
		if err != nil {
			t.Fatalf("Mark: %v", err)
		}
		return fresh
	}
	mark := func(t *testing.T, kind otlpreceiver.MarkerKind, calls ...otlpreceiver.Call) []bool {
		t.Helper()
		return markFor(t, tenant, kind, calls...)
	}

	t.Run("a call is fresh once", func(t *testing.T) {
		if got := mark(t, otlpreceiver.MarkerUsage, call(1, 1), call(1, 2)); !slices.Equal(got, []bool{true, true}) {
			t.Fatalf("first mark = %v, want both fresh", got)
		}
		if got := mark(t, otlpreceiver.MarkerUsage, call(1, 2), call(1, 3), call(1, 1)); !slices.Equal(got, []bool{false, true, false}) {
			t.Fatalf("second mark = %v, want only the new span fresh", got)
		}
	})

	t.Run("answers keep the order of calls across traces", func(t *testing.T) {
		mark(t, otlpreceiver.MarkerUsage, call(2, 1), call(3, 2))
		got := mark(t, otlpreceiver.MarkerUsage, call(2, 1), call(3, 1), call(2, 2), call(3, 2), call(4, 1))
		if want := []bool{false, true, true, false, true}; !slices.Equal(got, want) {
			t.Fatalf("mark = %v, want %v", got, want)
		}
	})

	t.Run("the same span id in another trace is another call", func(t *testing.T) {
		mark(t, otlpreceiver.MarkerUsage, call(5, 1))
		if got := mark(t, otlpreceiver.MarkerUsage, call(6, 1)); !slices.Equal(got, []bool{true}) {
			t.Fatalf("mark = %v: span 1 of trace 6 taken for span 1 of trace 5", got)
		}
	})

	t.Run("usage and content are marked independently", func(t *testing.T) {
		mark(t, otlpreceiver.MarkerUsage, call(7, 1))
		for _, step := range []struct {
			kind otlpreceiver.MarkerKind
			want bool
			why  string
		}{
			{otlpreceiver.MarkerContent, true, "content not fresh for a call whose usage is marked"},
			{otlpreceiver.MarkerContent, false, "content fresh twice"},
			{otlpreceiver.MarkerUsage, false, "marking content cleared the usage marker"},
		} {
			if got := mark(t, step.kind, call(7, 1)); !slices.Equal(got, []bool{step.want}) {
				t.Fatalf("mark = %v: %s", got, step.why)
			}
		}
	})

	t.Run("unmark makes a call fresh again", func(t *testing.T) {
		mark(t, otlpreceiver.MarkerUsage, call(8, 1), call(8, 2), call(9, 1))
		if err := m.Unmark(t.Context(), tenant, otlpreceiver.MarkerUsage, []otlpreceiver.Call{call(8, 2), call(9, 1)}); err != nil {
			t.Fatalf("Unmark: %v", err)
		}
		if got := mark(t, otlpreceiver.MarkerUsage, call(8, 1), call(8, 2), call(9, 1)); !slices.Equal(got, []bool{false, true, true}) {
			t.Fatalf("mark after unmark = %v, want only the unmarked calls fresh", got)
		}
	})

	t.Run("a whole export of one trace", func(t *testing.T) {
		calls := make([]otlpreceiver.Call, otlpreceiver.MaxRecords)
		for i := range calls {
			calls[i] = call(10, i+1)
		}
		if got := mark(t, otlpreceiver.MarkerUsage, calls...); slices.Contains(got, false) {
			t.Fatal("a call of a first export reported as already marked")
		}
		if got := mark(t, otlpreceiver.MarkerUsage, calls...); slices.Contains(got, true) {
			t.Fatal("a call of a resent export reported as fresh")
		}
		if err := m.Unmark(t.Context(), tenant, otlpreceiver.MarkerUsage, calls); err != nil {
			t.Fatalf("Unmark: %v", err)
		}
	})

	t.Run("nothing to mark", func(t *testing.T) {
		if got := mark(t, otlpreceiver.MarkerUsage); len(got) != 0 {
			t.Fatalf("mark of no calls = %v", got)
		}
	})

	t.Run("tenants do not see each other's markers", func(t *testing.T) {
		mark(t, otlpreceiver.MarkerUsage, call(12, 1))
		if got := markFor(t, "project-b", otlpreceiver.MarkerUsage, call(12, 1)); !slices.Equal(got, []bool{true}) {
			t.Fatalf("mark = %v: a call reported by one tenant is taken as recorded for another", got)
		}
		if err := m.Unmark(t.Context(), "project-b", otlpreceiver.MarkerUsage, []otlpreceiver.Call{call(12, 1)}); err != nil {
			t.Fatalf("Unmark: %v", err)
		}
		if got := mark(t, otlpreceiver.MarkerUsage, call(12, 1)); !slices.Equal(got, []bool{false}) {
			t.Fatalf("mark = %v: one tenant's unmark removed another's marker", got)
		}
	})
}

func TestMarkers_Mem(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	runMarkersSuite(t, s)
}

// scripterOnly hides the batch interface of the store behind it.
type scripterOnly struct{ inner kv.Scripter }

func (s scripterOnly) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	return s.inner.RunScript(ctx, name, script, keys, args...)
}

func TestMarkers_StoreWithoutBatching(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	// Registers the in-memory scripts, which the wrapper keeps NewMarkers from doing.
	otlpreceiver.NewMarkers(s)
	runMarkersSuite(t, scripterOnly{inner: s})
}

// recorder notes each script call a store is asked to run and answers every one with reply.
type recorder struct {
	calls []string
	reply string
}

func (r *recorder) RunScript(_ context.Context, name, _ string, keys []string, args ...any) ([]byte, error) {
	r.calls = append(r.calls, fmt.Sprintf("%s %v %v", name, keys, args))
	return []byte(r.reply), nil
}

// One script call per trace, its keys under the tenant-and-trace hash tag, set for a day.
func TestMarkers_KeysAndScriptCalls(t *testing.T) {
	r := &recorder{reply: "1"}
	m := otlpreceiver.NewMarkers(r)
	calls := []otlpreceiver.Call{call(1, 1), call(2, 1), call(1, 2)}
	one, two := "{otlp:project-a:"+call(1, 1).TraceID+"}", "{otlp:project-a:"+call(2, 1).TraceID+"}"

	fresh, err := m.Mark(t.Context(), tenant, otlpreceiver.MarkerContent, calls)
	// Trace 1 asked about two keys and got one answer: an error, and its calls recorded rather than lost.
	if want := "otlpreceiver: mark returned 1 results for 2 keys"; err == nil || err.Error() != want || !slices.Equal(fresh, []bool{true, true, true}) {
		t.Fatalf("fresh = %v err = %v, want all fresh and %q", fresh, err, want)
	}
	if err := m.Unmark(t.Context(), tenant, otlpreceiver.MarkerUsage, calls[1:]); err != nil {
		t.Fatalf("Unmark: %v", err)
	}
	want := []string{
		"otlpreceiver.mark [" + one + ":content:0000000000000001 " + one + ":content:0000000000000002] [86400000]",
		"otlpreceiver.mark [" + two + ":content:0000000000000001] [86400000]",
		"otlpreceiver.unmark [" + two + ":usage:0000000000000001] []",
		"otlpreceiver.unmark [" + one + ":usage:0000000000000002] []",
	}
	if diff := cmp.Diff(want, r.calls); diff != "" {
		t.Fatalf("script calls (-want +got):\n%s", diff)
	}
}

// failingTrace fails every script call that touches one trace, like one unreachable shard.
type failingTrace struct {
	inner kv.Scripter
	trace string
}

var errStoreDown = errors.New("kv down")

func (s failingTrace) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	for _, k := range keys {
		if strings.Contains(k, s.trace) {
			return nil, errStoreDown
		}
	}
	return s.inner.RunScript(ctx, name, script, keys, args...)
}

func TestMarkers_FailingStoreReportsCallsAsFresh(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	healthy := otlpreceiver.NewMarkers(s)
	if _, err := healthy.Mark(t.Context(), tenant, otlpreceiver.MarkerUsage, []otlpreceiver.Call{call(1, 1), call(2, 1)}); err != nil {
		t.Fatal(err)
	}

	m := otlpreceiver.NewMarkers(failingTrace{inner: s, trace: call(1, 1).TraceID})
	fresh, err := m.Mark(t.Context(), tenant, otlpreceiver.MarkerUsage, []otlpreceiver.Call{call(1, 1), call(2, 1), call(1, 2)})
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("err = %v, want the store's error", err)
	}
	// Trace 1 could not be checked, so its calls are recorded rather than lost; trace 2 was answered.
	if want := []bool{true, false, true}; !slices.Equal(fresh, want) {
		t.Fatalf("fresh = %v, want %v", fresh, want)
	}
}
