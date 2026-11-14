package keypool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func opened(reason CooldownReason, step int, d time.Duration) *CircuitRecord {
	return &CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(d), BackoffStep: step, LastTransition: t0, Reason: reason}
}

// Each failure kind leaves the same record whether written by the script, queued on a caller's batch, or applied from Go on a store without scripts.
func TestRecordFailure_PerKind(t *testing.T) {
	ctx := context.Background()
	writers := map[string]func(*kv.Mem) *Selector{
		"script":     func(m *kv.Mem) *Selector { return New(m, noopLogger(), frozenClock(t0), nil) },
		"no scripts": func(m *kv.Mem) *Selector { return New(noScripts{m}, noopLogger(), frozenClock(t0), nil) },
	}
	for _, tc := range []struct {
		name       string
		hash       string
		kind       FailureKind
		retryAfter time.Duration
		times      int
		want       *CircuitRecord
	}{
		{"401", "h", FailureAuth, 0, 1, opened(ReasonUpstreamAuthFailed, 1, 30*time.Second)},
		{"403", "h", FailureForbidden, 0, 1, opened(ReasonUpstreamForbidden, 1, time.Second)},
		{"429 with a long retry-after", "h", FailureRateLimitLong, 30 * time.Second, 1, opened(ReasonUpstreamRateLimited, 0, 30*time.Second)},
		{"429 with a short retry-after", "h", FailureRateLimitShort, 2 * time.Second, 3, nil},
		{"5xx below the threshold", "h", FailureServerError, 0, DefaultFailureThreshold - 1, &CircuitRecord{Failures: DefaultFailureThreshold - 1, WindowStart: t0}},
		{"5xx at the threshold", "h", FailureServerError, 0, DefaultFailureThreshold, opened(ReasonUpstreamServerError, 1, time.Second)},
		{"network error below the threshold", "h", FailureNetwork, 0, 1, &CircuitRecord{Failures: 1, WindowStart: t0}},
		{"network error at the threshold", "h", FailureNetwork, 0, DefaultFailureThreshold, opened(ReasonUpstreamNetworkError, 1, time.Second)},
		{"dial failure is the host's", "h", FailureUpstreamUnreachable, 0, DefaultFailureThreshold, nil},
		{"unknown kind", "h", FailureKind(99), 0, 1, nil},
		{"anonymous key", hostkey.AnonIDPrefix + "host", FailureAuth, 0, 1, nil},
	} {
		for name, newSelector := range writers {
			sel := newSelector(kv.NewMem())
			for range tc.times {
				sel.RecordFailure(ctx, tc.hash, tc.kind, tc.retryAfter)
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) { wantRecord(t, sel, tc.hash, tc.want) })
		}
		t.Run(tc.name+"/prepared", func(t *testing.T) {
			sel, _ := newSel(t)
			for range tc.times {
				b := sel.NewBatch(1)
				p := sel.PrepareRecordFailure(b, tc.hash, tc.kind, tc.retryAfter)
				if (p != nil) != (tc.want != nil) {
					t.Fatalf("PrepareRecordFailure queued a script: %v, want %v", p != nil, tc.want != nil)
				}
				if p != nil {
					b.Exec(ctx)
					p.Finish(ctx)
				}
			}
			wantRecord(t, sel, tc.hash, tc.want)
		})
	}
	if p := New(noScripts{kv.NewMem()}, noopLogger(), nil, nil).PrepareRecordFailure(nil, "h", FailureAuth, 0); p != nil {
		t.Error("PrepareRecordFailure queued a script on a store that runs none")
	}
}

// Each failed probe opens the key for the next step of its ladder, capped at the last; each success then takes one step back down.
func TestRecordFailure_ClimbsTheLadderAndSuccessDecays(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		kind   FailureKind
		ladder []time.Duration
	}{
		{"401", FailureAuth, authCooldowns[:]},
		{"403", FailureForbidden, backoffSchedule[:]},
	} {
		sel, _ := newSel(t)
		ks := keys("h")
		now := t0
		var got []time.Duration
		for range len(tc.ladder) + 2 {
			sel.RecordFailure(ctx, "h", tc.kind, 0)
			rec, _ := sel.ReadCircuit(ctx, "h")
			got = append(got, rec.OpenUntil.Sub(now))
			if _, err := sel.Pick(ctx, "p", KeySelectionPrioritized, ks); err != ErrNoHealthyKeys {
				t.Fatalf("%s: pick inside the cooldown = %v, want ErrNoHealthyKeys", tc.name, err)
			}
			now = rec.OpenUntil
			sel.clock = frozenClock(now)
			if c, err := sel.PickCandidate(ctx, "p", KeySelectionPrioritized, ks, nil); err != nil || c.Record.State != CircuitHalfOpen {
				t.Fatalf("%s: pick at the cooldown's end = %+v, %v; want the probe", tc.name, c, err)
			}
		}
		last := tc.ladder[len(tc.ladder)-1]
		if d := cmp.Diff(append(append([]time.Duration(nil), tc.ladder...), last, last), got); d != "" {
			t.Errorf("%s: cooldowns (-want +got):\n%s", tc.name, d)
		}

		var steps []int
		for range len(tc.ladder) + 1 {
			sel.RecordSuccess(ctx, "h")
			rec, _ := sel.ReadCircuit(ctx, "h")
			if rec.State != CircuitClosed || rec.Reason != "" || !rec.ProbeUntil.IsZero() {
				t.Fatalf("%s: after a success: %+v, want a clean closed record", tc.name, rec)
			}
			steps = append(steps, rec.BackoffStep)
		}
		want := make([]int, len(tc.ladder)+1)
		for i := range tc.ladder {
			want[i] = len(tc.ladder) - 1 - i
		}
		if d := cmp.Diff(want, steps); d != "" {
			t.Errorf("%s: backoff steps over successes (-want +got):\n%s", tc.name, d)
		}
	}
}

// A key's record exists only once it has failed: a success on a key without one writes nothing, by any writer.
func TestRecordSuccess_WritesNoRecordForAKeyThatNeverFailed(t *testing.T) {
	ctx := context.Background()
	sel, mem := newSel(t)
	sel.RecordSuccess(ctx, "direct")
	wantRecord(t, sel, "direct", nil)

	b := sel.NewBatch(1)
	p := sel.PrepareRecordSuccess(b, "prepared")
	b.Exec(ctx)
	if err := p.Err(); err != nil {
		t.Fatalf("prepared success: %v", err)
	}
	p.Finish(ctx)
	wantRecord(t, sel, "prepared", nil)

	bare := New(noScripts{mem}, noopLogger(), frozenClock(t0), nil)
	bare.RecordSuccess(ctx, "bare")
	wantRecord(t, bare, "bare", nil)
	if p := bare.PrepareRecordSuccess(nil, "bare"); p != nil {
		t.Error("PrepareRecordSuccess queued a script on a store that runs none")
	}
}

// A probe's success closes the key the same way by any writer.
func TestRecordSuccess_ClosesAProbedKey(t *testing.T) {
	ctx := context.Background()
	for name, succeed := range map[string]func(sel *Selector){
		"script": func(sel *Selector) { sel.RecordSuccess(ctx, "h") },
		"prepared": func(sel *Selector) {
			b := sel.NewBatch(1)
			p := sel.PrepareRecordSuccess(b, "h")
			b.Exec(ctx)
			p.Finish(ctx)
		},
		"no scripts": func(sel *Selector) {
			bare := New(noScripts{sel.state}, noopLogger(), sel.clock, nil)
			bare.RecordSuccess(ctx, "h")
		},
	} {
		sel, _ := newSel(t)
		sel.RecordFailure(ctx, "h", FailureAuth, 0)
		at(sel, 30*time.Second)
		if _, err := sel.Pick(ctx, "p", KeySelectionPrioritized, keys("h")); err != nil {
			t.Fatalf("%s: probe: %v", name, err)
		}
		at(sel, 31*time.Second)
		succeed(sel)
		t.Run(name, func(t *testing.T) {
			wantRecord(t, sel, "h", &CircuitRecord{OpenUntil: t0.Add(30 * time.Second), LastTransition: t0.Add(31 * time.Second)})
		})
	}
}

// cancelAwareMem refuses scripts on a cancelled context, as a networked store does.
type cancelAwareMem struct{ *kv.Mem }

func (m cancelAwareMem) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.Mem.RunScript(ctx, name, script, keys, args...)
}

// A failure seen by a request whose caller has since gone is still written.
func TestRecordFailure_WritesAfterCallerLeft(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	RegisterScripts(mem)
	sel := New(cancelAwareMem{mem}, noopLogger(), frozenClock(t0), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sel.RecordFailure(ctx, "h", FailureAuth, 0)
	wantRecord(t, sel, "h", opened(ReasonUpstreamAuthFailed, 1, 30*time.Second))
}

func TestReadCircuit_AndClearCircuit(t *testing.T) {
	ctx := context.Background()
	sel, mem := newSel(t)
	wantRecord(t, sel, "h", nil)

	if err := mem.Set(ctx, circuitKey("h"), []byte("not json"), 0); err != nil {
		t.Fatal(err)
	}
	wantRecord(t, sel, "h", nil)

	sel.RecordFailure(ctx, "h", FailureAuth, 0)
	sel.RecordFailure(ctx, "other", FailureAuth, 0)
	if err := ClearCircuit(ctx, mem, "h"); err != nil {
		t.Fatal(err)
	}
	wantRecord(t, sel, "h", nil)
	wantRecord(t, sel, "other", opened(ReasonUpstreamAuthFailed, 1, 30*time.Second))
}

// scriptsDown fails every breaker script as a store that took it and never answered; reads work.
type scriptsDown struct{ kvtest.ScriptStore }

func (scriptsDown) RunScript(context.Context, string, string, []string, ...any) ([]byte, error) {
	return nil, kv.ErrUnavailable
}

// With the store refusing every breaker write, a pod's pick state still applies its own transitions: counted failures open the key, the key is probed on the record the pod holds, and a success clears a count.
func TestSelector_AppliesTransitionsAloneWhenScriptsFail(t *testing.T) {
	ctx := context.Background()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	now := t0
	p := newPod(scriptsDown{mem}, &now)
	p.sel.WithBreaker(2, time.Minute)
	ks := keys("hA", "hB")
	pick := func() Candidate {
		t.Helper()
		c, err := p.sel.PickCandidate(ctx, "scope", KeySelectionPrioritized, ks, nil)
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		return c
	}

	p.sel.RecordFailure(ctx, "hA", FailureServerError, 0)
	if c := pick(); c.Key != ks[0] || !c.SuccessNeedsWrite() {
		t.Fatalf("after one counted failure picked %+v, want hA with a count to clear", c)
	}
	p.sel.RecordSuccess(ctx, "hA")
	if c := pick(); c.Key != ks[0] || c.SuccessNeedsWrite() {
		t.Fatalf("after the success picked %+v, want hA with nothing to clear", c)
	}

	p.sel.RecordFailure(ctx, "hA", FailureServerError, 0)
	p.sel.RecordFailure(ctx, "hA", FailureServerError, 0)
	if c := pick(); c.Key != ks[1] {
		t.Fatalf("after the threshold picked %s, want hB", c.Key.KeyHash)
	}
	now = t0.Add(backoffSchedule[0])
	want := Candidate{Key: ks[0], Stored: true, Record: CircuitRecord{
		State: CircuitHalfOpen, OpenUntil: now, BackoffStep: 1, LastTransition: t0, Reason: ReasonUpstreamServerError,
	}}
	if d := cmp.Diff(want, pick(), cmp.AllowUnexported(hostkey.HostKey{})); d != "" {
		t.Fatalf("pick at the cooldown's end, claim script failing (-want +got):\n%s", d)
	}
	if _, found := p.sel.ReadCircuit(ctx, "hA"); found {
		t.Fatal("a failed script left a record in the store")
	}
}

// Concurrent successes, failures and reads of one key never leave or observe a record outside the three states.
func TestRecordSuccessAndFailure_ConcurrentWritesStayWhole(t *testing.T) {
	sel, _ := newSel(t)
	ctx := context.Background()
	valid := func(rec CircuitRecord) bool {
		return rec.State == CircuitClosed || rec.State == CircuitOpen || rec.State == CircuitHalfOpen
	}
	var wg sync.WaitGroup
	for _, fn := range []func(){
		func() { sel.RecordSuccess(ctx, "h") },
		func() { sel.RecordFailure(ctx, "h", FailureServerError, 0) },
		func() {
			if rec, _ := sel.ReadCircuit(ctx, "h"); !valid(rec) || rec.Failures >= DefaultFailureThreshold {
				t.Errorf("read a torn record: %+v", rec)
			}
		},
	} {
		wg.Go(func() {
			for range 200 {
				fn()
			}
		})
	}
	wg.Wait()
	if rec, found := sel.ReadCircuit(ctx, "h"); !found || !valid(rec) {
		t.Fatalf("final record = %+v (found %v), want a whole record", rec, found)
	}
}
