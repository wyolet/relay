package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// lostReply answers every reserve with failure: by default it runs the reserve first, as a store whose reply never arrived; skip leaves it unrun.
type lostReply struct {
	kvtest.ScriptStore
	skip bool
	fail error
}

func (s lostReply) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if name != "limit.reserve" {
		return s.ScriptStore.RunScript(ctx, name, script, keys, args...)
	}
	if !s.skip {
		if _, err := s.ScriptStore.RunScript(ctx, name, script, keys, args...); err != nil {
			return nil, err
		}
	}
	return nil, s.fail
}

var (
	errReplyTimeout = fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)
	errNotSent      = fmt.Errorf("%w: %w: %w", kv.ErrUnavailable, kv.ErrNotRun, context.DeadlineExceeded)
)

func TestReleaseUnansweredMem(t *testing.T) {
	runReleaseUnansweredSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := newTestStore(t)
		RegisterScripts(m)
		return m
	})
}

// runReleaseUnansweredSuite pins the release of a reserve whose reply was lost: the lease it may hold is freed, its bucket charge stays, and a reserve that never ran or runs after the release holds nothing.
func runReleaseUnansweredSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	ctx := context.Background()
	const window = time.Minute
	released := unansweredReleases.WithLabelValues("released")

	// post reaches the store directly, as the post-flight client does; l is the admission limiter whose reply is lost.
	setup := func(t *testing.T, wrap lostReply) (l, post *Limiter) {
		store := newStore(t)
		post = New(store, discardLog(), nil)
		wrap.ScriptStore = store
		return New(wrap, discardLog(), nil).ReleaseUnansweredOn(post), post
	}
	granted := func(t *testing.T, l *Limiter, scope string, rules []Rule) *Reservation {
		t.Helper()
		res, err := l.Reserve(ctx, scope, rules)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		return res
	}
	refusedBy := func(t *testing.T, l *Limiter, scope string, rules []Rule, key string) {
		t.Helper()
		_, err := l.Reserve(ctx, scope, rules)
		var ee *ExceededError
		if !errors.As(err, &ee) || ee.Rule.Key != key {
			t.Fatalf("reserve: err = %v, want refused by %q", err, key)
		}
	}

	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply_lost_frees_lease/batched=%v", batched), func(t *testing.T) {
			l, post := setup(t, lostReply{fail: errReplyTimeout})
			const scope = "unanswered-lost"
			con := conRule("unanswered:lost:con", "concurrency", 1, window)
			before := testutil.ToFloat64(released)

			var err error
			if batched {
				b := kv.NewBatch(l.store, 1)
				p, perr := l.PrepareReserve(b, scope, []Rule{con})
				if perr != nil {
					t.Fatal(perr)
				}
				b.Exec(ctx)
				_, err = p.Finish(ctx)
			} else {
				_, err = l.Reserve(ctx, scope, []Rule{con})
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("lost reserve: err = %v, want ErrUnavailable", err)
			}
			l.releases.Wait()
			if got := testutil.ToFloat64(released) - before; got != 1 {
				t.Fatalf("released counter moved by %v, want 1", got)
			}
			granted(t, post, scope, []Rule{con})
			refusedBy(t, post, scope, []Rule{con}, con.Key)
		})
	}

	t.Run("reply_lost_keeps_charge", func(t *testing.T) {
		l, post := setup(t, lostReply{fail: errReplyTimeout})
		const scope = "unanswered-charge"
		con := conRule("unanswered:charge:con", "concurrency", 1, window)
		req := reqRule("unanswered:charge:req", "requests", 1, window)
		if _, err := l.Reserve(ctx, scope, []Rule{con, req}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("lost reserve: err = %v, want ErrUnavailable", err)
		}
		l.releases.Wait()
		refusedBy(t, post, scope, []Rule{con, req}, req.Key)
		granted(t, post, scope, []Rule{con})
	})

	t.Run("never_ran_leaves_other_leases", func(t *testing.T) {
		l, post := setup(t, lostReply{skip: true, fail: errReplyTimeout})
		const scope = "unanswered-unrun"
		con := conRule("unanswered:unrun:con", "concurrency", 1, window)
		holder := granted(t, post, scope, []Rule{con})
		if _, err := l.Reserve(ctx, scope, []Rule{con}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unrun reserve: err = %v, want ErrUnavailable", err)
		}
		l.releases.Wait()
		refusedBy(t, post, scope, []Rule{con}, con.Key)
		if err := post.Commit(ctx, holder, Observations{}); err != nil {
			t.Fatal(err)
		}
		granted(t, post, scope, []Rule{con})
	})

	t.Run("ran_after_release_takes_nothing", func(t *testing.T) {
		l, post := setup(t, lostReply{fail: errReplyTimeout})
		const scope = "unanswered-late"
		con := conRule("unanswered:late:con", "concurrency", 1, window)
		req := reqRule("unanswered:late:req", "requests", 1, window)
		rules := []Rule{con, req}
		call, err := post.buildReserveCall(scope, rules, "late-token")
		if err != nil {
			t.Fatal(err)
		}
		l.releaseUnanswered(scope, rules, "late-token", errReplyTimeout)
		l.releases.Wait()

		raw, err := post.runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
		if err != nil {
			t.Fatalf("late reserve: %v", err)
		}
		var out reserveResult
		if err := json.Unmarshal(raw, &out); err != nil || !out.Released {
			t.Fatalf("late reserve = %s (%v), want released", raw, err)
		}
		granted(t, post, scope, rules)
		refusedBy(t, post, scope, rules, con.Key)
	})

	t.Run("not_run_sends_no_release", func(t *testing.T) {
		store := newStore(t)
		rec := kvtest.NewRecording(store)
		l := New(lostReply{ScriptStore: store, skip: true, fail: errNotSent}, discardLog(), nil).
			ReleaseUnansweredOn(New(rec, discardLog(), nil))
		con := conRule("unanswered:notrun:con", "concurrency", 1, window)
		if _, err := l.Reserve(ctx, "unanswered-notrun", []Rule{con}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reserve: err = %v, want ErrUnavailable", err)
		}
		l.releases.Wait()
		if n := len(rec.Named("limit.commit")); n != 0 {
			t.Fatalf("release scripts = %d, want 0", n)
		}
	})
}

// A reserve with no concurrency rule holds no lease, so a lost reply sends no release.
func TestReleaseUnansweredSkipsLeaseFreeReserve(t *testing.T) {
	m := newTestStore(t)
	RegisterScripts(m)
	rec := kvtest.NewRecording(m)
	l := New(lostReply{ScriptStore: m, fail: errReplyTimeout}, discardLog(), nil).ReleaseUnansweredOn(New(rec, discardLog(), nil))
	if _, err := l.Reserve(context.Background(), "unanswered-nolease", []Rule{reqRule("r", "r", 10, time.Minute)}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reserve: err = %v, want ErrUnavailable", err)
	}
	l.releases.Wait()
	if n := len(rec.Named("limit.commit")); n != 0 {
		t.Fatalf("release scripts = %d, want 0", n)
	}
}

// A reserve refused at the wait-queue bound was never sent, so it must not release anything.
func TestReleaseUnansweredSkipsPoolBusy(t *testing.T) {
	if mayHaveRun(kv.ErrPoolBusy) {
		t.Fatal("mayHaveRun(ErrPoolBusy) = true, want false")
	}
	if !errors.Is(kv.ErrPoolBusy, kv.ErrUnavailable) {
		t.Fatal("ErrPoolBusy is not ErrUnavailable")
	}
}
