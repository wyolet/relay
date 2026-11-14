package secret

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wyolet/relay/app/keypool"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeRefresher answers every refresh alike, counting them; when block is set a refresh waits for it to close or for its own context to end.
type fakeRefresher struct {
	value   string
	changed bool
	err     error
	block   chan struct{}

	calls atomic.Int32
	keys  sync.Map
}

func (f *fakeRefresher) Refresh(ctx context.Context, keyID string) (string, bool, error) {
	f.calls.Add(1)
	f.keys.Store(keyID, true)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	return f.value, f.changed, f.err
}

// Only a 401 on the last candidate waits for a re-resolve, and only a value that changed is retried: a 403 says nothing about rotation, and a revoked key resolves to the same value.
func TestAgentVerdicts(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name          string
		value         string
		changed       bool
		err           error
		kind          keypool.FailureKind
		more          bool
		want          keypool.Verdict
		wantFresh     string
		wantRefreshes int32
	}{
		{"server error, more candidates", "rotated", true, nil, keypool.FailureServerError, true, keypool.VerdictNext, "", 0},
		{"server error, last candidate", "rotated", true, nil, keypool.FailureServerError, false, keypool.VerdictFail, "", 0},
		{"forbidden, more candidates", "rotated", true, nil, keypool.FailureForbidden, true, keypool.VerdictNext, "", 0},
		{"forbidden, last candidate", "rotated", true, nil, keypool.FailureForbidden, false, keypool.VerdictFail, "", 0},
		{"auth, more candidates: fail over and heal behind", "rotated", true, nil, keypool.FailureAuth, true, keypool.VerdictNext, "", 1},
		{"auth, last candidate, rotated", "rotated", true, nil, keypool.FailureAuth, false, keypool.VerdictRetry, "rotated", 1},
		{"auth, last candidate, unchanged", "same", false, nil, keypool.FailureAuth, false, keypool.VerdictFail, "", 1},
		{"auth, last candidate, refresh failing", "rotated", true, boom, keypool.FailureAuth, false, keypool.VerdictFail, "", 1},
	} {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeRefresher{value: tc.value, changed: tc.changed, err: tc.err}
			v, fresh := NewAgent(f, time.Second, discard()).OnFailure(context.Background(), "k", tc.kind, tc.more)
			synctest.Wait()
			if _, asked := f.keys.Load("k"); v != tc.want || fresh != tc.wantFresh || f.calls.Load() != tc.wantRefreshes || asked != (tc.wantRefreshes > 0) {
				t.Errorf("%s: verdict %v fresh %q after %d refreshes, want %v %q after %d of key k", tc.name, v, fresh, f.calls.Load(), tc.want, tc.wantFresh, tc.wantRefreshes)
			}
		})
	}
}

// A caller that gives up stops waiting at once; the refresh itself is bounded by the agent's timeout, not the caller's context, so a later caller still gets its answer.
func TestAgentWaitIsBoundedByCallerAndTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRefresher{value: "rotated", changed: true, block: make(chan struct{})}
		a := NewAgent(f, 30*time.Second, discard())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if v, fresh := a.OnFailure(ctx, "k", keypool.FailureAuth, false); v != keypool.VerdictFail || fresh != "" {
			t.Errorf("a caller that gave up: %v %q, want Fail", v, fresh)
		}
		started := time.Now()
		if v, fresh := a.OnFailure(context.Background(), "k", keypool.FailureAuth, false); v != keypool.VerdictFail || fresh != "" || time.Since(started) != 30*time.Second {
			t.Errorf("a refresh that never answers: %v %q after %v, want Fail at the 30s timeout", v, fresh, time.Since(started))
		}
		if n := f.calls.Load(); n != 1 {
			t.Errorf("refreshes = %d, want the second caller to join the one in flight", n)
		}
		// NewAgent's default timeout stands in for a zero one.
		started = time.Now()
		NewAgent(f, 0, nil).OnFailure(context.Background(), "k", keypool.FailureAuth, false)
		if time.Since(started) != 5*time.Second {
			t.Errorf("default timeout = %v, want 5s", time.Since(started))
		}
	})
}

// Concurrent last-candidate auth failures of one key share one refresh and all retry with its value; another key refreshes on its own.
func TestAgentSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRefresher{value: "rotated", changed: true, block: make(chan struct{})}
		a := NewAgent(f, time.Minute, discard())
		var retried atomic.Int32
		for i := range 21 {
			keyID := "samekey"
			if i == 20 {
				keyID = "otherkey"
			}
			go func() {
				if v, fresh := a.OnFailure(context.Background(), keyID, keypool.FailureAuth, false); v == keypool.VerdictRetry && fresh == "rotated" {
					retried.Add(1)
				}
			}()
		}
		synctest.Wait()
		if n := f.calls.Load(); n != 2 || retried.Load() != 0 {
			t.Fatalf("with every caller parked: %d refreshes, %d answered; want one refresh per key and none answered", n, retried.Load())
		}
		close(f.block)
		synctest.Wait()
		if f.calls.Load() != 2 || retried.Load() != 21 {
			t.Errorf("%d refreshes, %d callers retried with the fresh value; want 2 and 21", f.calls.Load(), retried.Load())
		}
	})
}
