package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

type quota struct{ Limit int }

// withQuota registers a controller producing a quota per team that fails while
// a team's limit is negative, panics at -2, and returns ErrRetry at -3.
func withQuota(f *fixture) {
	engine.RegisterKind(f.e, engine.KindSpec[quota]{Kind: "quota"})
	c := engine.NewController("quota", "quota", func(tx *engine.Txn, id string) error {
		f.count("quota/" + id)
		tm := engine.Get[team](tx, id)
		if tm == nil {
			engine.Drop(tx)
			return nil
		}
		switch tm.Limit {
		case -1:
			return errBoom
		case -2:
			panic("bad limit")
		case -3:
			return engine.ErrRetry
		case -4:
			engine.Put(tx, &quota{})
			engine.Put(tx, &quota{})
			return nil
		}
		engine.Put(tx, &quota{Limit: tm.Limit})
		return nil
	})
	engine.Watch(c, func(t *team) []string { return []string{t.ID} })
	f.e.Register(c)
}

func TestReconcileErrorKeepsPreviousOutput(t *testing.T) {
	f := newFixture(t, engine.Options{Debounce: time.Hour}, withQuota)
	ref := engine.Ref{Kind: "quota", ID: "t1"}
	for _, limit := range []int{-1, -2, -4} {
		mustApply(t, f.e, &team{ID: "t1", Name: "alpha", Limit: limit})
		v := f.view()
		if q := engine.Get[quota](v, "t1"); q == nil || q.Limit != 10 {
			t.Fatalf("limit %d: previous output not kept: %+v", limit, q)
		}
		if got := codes(v.Issues(ref)); !slices.Equal(got, []string{"reconcile_failed"}) {
			t.Fatalf("limit %d: issues = %v", limit, got)
		}
	}
	f.mu.Lock()
	n := len(f.errs)
	panicked := strings.Contains(f.errs[1].Error(), "bad limit")
	f.mu.Unlock()
	if n != 3 || !panicked {
		t.Fatalf("OnReconcileError calls = %d, panic surfaced = %v", n, panicked)
	}

	mustApply(t, f.e, &team{ID: "t1", Name: "alpha", Limit: 12})
	v := f.view()
	if engine.Get[quota](v, "t1").Limit != 12 || len(v.Issues(ref)) != 0 {
		t.Fatal("a successful reconcile did not clear the failure")
	}
}

func TestErrRetryIsRetriedWithoutNewEvents(t *testing.T) {
	f := newFixture(t, engine.Options{Debounce: time.Millisecond}, withQuota)
	mustApply(t, f.e, &team{ID: "t1", Name: "alpha", Limit: -3})
	deadline := time.Now().Add(5 * time.Second)
	for f.callsOf("quota/t1") < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("retries = %d", f.callsOf("quota/t1"))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCycleIsAbandoned(t *testing.T) {
	var abandoned atomic.Int32
	f := newFixture(t, engine.Options{MaxRounds: 4, Hooks: engine.Hooks{
		OnBatchAbandoned: func(engine.Generation, int) { abandoned.Add(1) },
	}}, func(f *fixture) {
		type ping struct{ N int }
		type pong struct{ N int }
		engine.RegisterKind(f.e, engine.KindSpec[ping]{Kind: "ping"})
		engine.RegisterKind(f.e, engine.KindSpec[pong]{Kind: "pong"})
		a := engine.NewController("ping", "ping", func(tx *engine.Txn, id string) error {
			n := 0
			if p := engine.Get[pong](tx, id); p != nil {
				n = p.N + 1
			}
			engine.Put(tx, &ping{N: n})
			return nil
		})
		engine.Watch(a, func(u *user) []string { return []string{u.ID} })
		b := engine.NewController("pong", "pong", func(tx *engine.Txn, id string) error {
			n := 0
			if p := engine.Get[ping](tx, id); p != nil {
				n = p.N
			}
			engine.Put(tx, &pong{N: n})
			return nil
		})
		engine.Watch(b, func(u *user) []string { return []string{u.ID} })
		f.e.Register(a)
		f.e.Register(b)
	})
	if abandoned.Load() != 1 {
		t.Fatalf("OnBatchAbandoned calls = %d", abandoned.Load())
	}
	v := f.view()
	if got := codes(v.Issues(engine.Ref{Kind: "ping", ID: "u1"})); !slices.Equal(got, []string{"batch_abandoned"}) {
		t.Fatalf("issues = %v", got)
	}
	if s := f.lastStats(); s.Rounds != 4 {
		t.Fatalf("rounds = %d", s.Rounds)
	}
}

func TestNotifyCoalescesIntoOneGeneration(t *testing.T) {
	f := newFixture(t, engine.Options{Debounce: 20 * time.Millisecond}, nil)
	f.teams.put(&team{ID: "t1", Name: "alpha", Limit: 30, Ver: 5})
	f.users.put(&user{ID: "u9", Name: "zed", TeamID: "t2"})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t1"}, Op: engine.OpUpsert, Version: 4})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t1"}, Op: engine.OpUpsert, Version: 5})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "user", ID: "u9"}, Op: engine.OpUpsert})
	f.waitGen(2)
	v := f.view()
	if engine.Get[userPlan](v, "u1").Team.Limit != 30 || engine.Get[userPlan](v, "u9") == nil {
		t.Fatal("events not applied")
	}
	if f.teams.gets.Load() != 1 {
		t.Fatalf("team fetched %d times, want 1", f.teams.gets.Load())
	}

	// A version at or below the held one is dropped without a fetch.
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t1"}, Op: engine.OpUpsert, Version: 5})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "user", ID: "u3"}, Op: engine.OpDelete})
	f.waitGen(3)
	if f.teams.gets.Load() != 1 {
		t.Fatalf("stale event fetched: %d", f.teams.gets.Load())
	}
	if engine.Get[user](f.view(), "u3") != nil || f.view().Gen() != 3 {
		t.Fatal("delete event not applied in one generation")
	}
}

func TestFetchFailureIsRetried(t *testing.T) {
	f := newFixture(t, engine.Options{Debounce: time.Millisecond}, nil)
	f.teams.mu.Lock()
	f.teams.err = errBoom
	f.teams.mu.Unlock()
	f.teams.put(&team{ID: "t1", Name: "alpha", Limit: 42})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t1"}, Op: engine.OpUpsert})
	for f.teams.gets.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	f.teams.mu.Lock()
	f.teams.err = nil
	f.teams.mu.Unlock()
	f.waitGen(2)
	if engine.Get[team](f.view(), "t1").Limit != 42 {
		t.Fatal("event lost after a failed fetch")
	}
}

func TestResyncPublishesOnlyDifferences(t *testing.T) {
	var resyncs []int
	f := newFixture(t, engine.Options{Hooks: engine.Hooks{OnResync: func(n int, _ time.Duration) { resyncs = append(resyncs, n) }}}, nil)
	ctx := context.Background()
	if err := f.e.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	if f.view().Gen() != 1 {
		t.Fatal("unchanged resync published")
	}
	// Version 0 compares values: an equal copy is no change, a new value is.
	f.users.put(&user{ID: "u1", Name: "ann", TeamID: "t1"})
	f.users.put(&user{ID: "u2", Name: "bob", TeamID: "t2"})
	f.users.del("u3")
	if err := f.e.Resync(ctx); err != nil {
		t.Fatal(err)
	}
	v := f.view()
	if v.Gen() != 2 || engine.Get[user](v, "u3") != nil || engine.Get[userPlan](v, "u2").Team.ID != "t2" {
		t.Fatalf("resync result wrong at gen %d", v.Gen())
	}
	if !slices.Equal(resyncs, []int{0, 2}) {
		t.Fatalf("OnResync changed = %v", resyncs)
	}
	f.users.mu.Lock()
	f.users.err = errBoom
	f.users.mu.Unlock()
	if err := f.e.Resync(ctx); !errors.Is(err, errBoom) {
		t.Fatalf("resync error = %v", err)
	}
}

func TestCloseDrainsQueue(t *testing.T) {
	f := newFixture(t, engine.Options{Debounce: time.Hour}, nil)
	f.teams.put(&team{ID: "t1", Name: "alpha", Limit: 77})
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t1"}, Op: engine.OpUpsert})
	if err := f.e.Close(); err != nil {
		t.Fatal(err)
	}
	if engine.Get[team](f.view(), "t1").Limit != 77 {
		t.Fatal("Close did not drain the queue")
	}
	gen := f.view().Gen()
	if err := engine.Apply(f.e, &team{ID: "t2", Name: "beta"}); err == nil {
		t.Fatal("Apply after Close succeeded")
	}
	f.e.Notify(engine.Event{Ref: engine.Ref{Kind: "team", ID: "t2"}, Op: engine.OpDelete})
	if f.view().Gen() != gen {
		t.Fatal("published after Close")
	}
}

func TestRegistrationIsSealedAtLoad(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterKind after Load did not panic")
		}
	}()
	engine.RegisterKind(f.e, engine.KindSpec[window]{Kind: "late"})
}

func TestIssueSourceOverride(t *testing.T) {
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		engine.RegisterKind(f.e, engine.KindSpec[bool]{Kind: "check"})
		c := engine.NewController("check", "check", func(tx *engine.Txn, id string) error {
			if u := engine.Get[user](tx, id); u != nil && u.TeamID == "t2" {
				tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: "lonely", Source: engine.Ref{Kind: "user", ID: id}})
			}
			ok := true
			engine.Put(tx, &ok)
			return nil
		})
		engine.Watch(c, func(u *user) []string { return []string{u.ID} })
		f.e.Register(c)
	})
	v := f.view()
	if got := codes(v.Issues(engine.Ref{Kind: "user", ID: "u3"})); !slices.Equal(got, []string{"lonely"}) {
		t.Fatalf("issues on the source row = %v", got)
	}
	if len(v.Issues(engine.Ref{Kind: "check", ID: "u3"})) != 0 {
		t.Fatal("issue attached to Out despite an explicit Source")
	}
}
