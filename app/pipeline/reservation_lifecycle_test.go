package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

type observedCommit struct {
	guardKey  string
	cancelled bool
}

type observedKV struct {
	*kv.Mem
	mu       sync.Mutex
	commits  []observedCommit
	commitCh chan observedCommit
}

func newObservedKV() *observedKV {
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	// keypool.New only auto-registers on a bare *kv.Mem; this double embeds
	// one, so the atomic record-success emulator must be installed here.
	keypool.RegisterScripts(mem)
	return &observedKV{Mem: mem, commitCh: make(chan observedCommit, 8)}
}

// RunScriptBatch must shadow the embedded Mem's implementation: the batched
// commit path (Limiter.CommitBoth) would otherwise bypass the RunScript
// override below and the test would never observe its commits. Sequential
// per-call execution matches Mem semantics.
func (s *observedKV) RunScriptBatch(ctx context.Context, calls []kv.ScriptCall) []kv.ScriptResult {
	results := make([]kv.ScriptResult, len(calls))
	for i, c := range calls {
		v, err := s.RunScript(ctx, c.Name, c.Script, c.Keys, c.Args...)
		results[i] = kv.ScriptResult{Value: v, Err: err}
	}
	return results
}

func (s *observedKV) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	out, err := s.Mem.RunScript(ctx, name, script, keys, args...)
	if err == nil && name == "limit.commit" {
		rec := observedCommit{guardKey: keys[0], cancelled: args[6].(int64) == 1}
		s.mu.Lock()
		s.commits = append(s.commits, rec)
		s.mu.Unlock()
		s.commitCh <- rec
	}
	return out, err
}

func (s *observedKV) snapshotCommits() []observedCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]observedCommit, len(s.commits))
	copy(out, s.commits)
	return out
}

type reservationTestAdapter struct {
	call func(context.Context) (*http.Response, error)
}

func (a reservationTestAdapter) Call(ctx context.Context, _ string, _ *string, _ string, _ []byte, _ http.Header, _ string, _ bool, _ bool) (*http.Response, error) {
	if a.call != nil {
		return a.call(ctx)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
}

func (a reservationTestAdapter) ExtractTokens([]byte) pkgusage.Tokens {
	return pkgusage.Tokens{"input": 7, "output": 11}
}

func (a reservationTestAdapter) Retryable(*http.Response) (bool, keypool.FailureKind, time.Duration) {
	return false, 0, 0
}

type reservationHarness struct {
	pipeline *Pipeline
	store    *observedKV
	selector *keypool.Selector
	hops     []plan.RouteHop
	key      *hostkey.HostKey
}

// newReservationHarness holds a caller concurrency counter and the credential's account concurrency counter: two reservations, one commit each.
func newReservationHarness() reservationHarness {
	store := newObservedKV()
	selector := keypool.New(store, slog.Default(), nil, nil)
	key := testKey("host-key-1")
	hop := buildHop(keypool.KeySelectionPrioritized, callerCounter(concurrencyRateLimit("rl-inbound", "inbound")),
		testSlot{key: key, acc: accountPlan("acct-1"), upstream: accountCounters("acct-1", concurrencyRateLimit("rl-upstream", "upstream"))})
	return reservationHarness{
		pipeline: &Pipeline{Limiter: lua.New(pkgratelimit.New(store, slog.Default(), nil)), Selector: selector, Logger: slog.Default()},
		store:    store,
		selector: selector,
		hops:     []plan.RouteHop{hop},
		key:      key,
	}
}

func concurrencyRateLimit(id, name string) *appratelimit.RateLimit {
	return &appratelimit.RateLimit{
		Meta: meta.Metadata{ID: id, Name: name},
		Spec: appratelimit.Spec{Rules: []appratelimit.Rule{{
			Meter:    appratelimit.MeterConcurrency,
			Amount:   5,
			Window:   appratelimit.Window(time.Minute),
			Strategy: appratelimit.StrategySlidingWindow,
		}}},
	}
}

func waitForCommits(t *testing.T, store *observedKV, n int) []observedCommit {
	t.Helper()
	for i := range n {
		select {
		case <-store.commitCh:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for commit %d of %d", i+1, n)
		}
	}
	commits := store.snapshotCommits()
	if len(commits) != n {
		t.Fatalf("commit count = %d, want %d", len(commits), n)
	}
	return commits
}

func TestRunCancelAfterCallKeepsInboundAndHeldKeyCharge(t *testing.T) {
	t.Parallel()

	h := newReservationHarness()
	_, err := h.pipeline.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			return nil, context.Canceled
		}},
		Hops: h.hops,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}

	commits := h.store.snapshotCommits()
	if len(commits) != 2 {
		t.Fatalf("commit count = %d, want 2 (inbound + held key)", len(commits))
	}
	for _, commit := range commits {
		if commit.cancelled {
			t.Fatalf("commit %q used Cancelled=true, want the charge kept once the call was made", commit.guardKey)
		}
	}
	if rec, found := h.selector.ReadCircuit(context.Background(), h.key.KeyHash); found {
		t.Fatalf("circuit record after caller cancellation = %+v, want no RecordFailure", rec)
	}
}

func TestRunUnreachableHostCancelsInboundAndHeldKeyReservations(t *testing.T) {
	t.Parallel()

	h := newReservationHarness()
	dialErr := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	_, err := h.pipeline.Run(context.Background(), &Request{
		Adapter: reservationTestAdapter{call: func(context.Context) (*http.Response, error) {
			return nil, dialErr
		}},
		Hops: h.hops,
	})
	var unreachable *UpstreamUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("Run error = %v, want UpstreamUnreachableError", err)
	}

	commits := h.store.snapshotCommits()
	if len(commits) != 2 {
		t.Fatalf("commit count = %d, want 2 (inbound + held key rollback)", len(commits))
	}
	for _, commit := range commits {
		if !commit.cancelled {
			t.Fatalf("commit %q used Cancelled=false, want Cancelled=true rollback", commit.guardKey)
		}
	}
	if rec, found := h.selector.ReadCircuit(context.Background(), h.key.KeyHash); found {
		t.Fatalf("circuit record after host-unreachable failure = %+v, want no key RecordFailure", rec)
	}
}

func TestRunSuccessCommitsReservationsOnPostFlightOnce(t *testing.T) {
	t.Parallel()

	h := newReservationHarness()
	done := make(chan struct{})
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
		close(done)
		return nil, nil
	}})
	h.pipeline.Lifecycle = reg

	res, err := h.pipeline.Run(context.Background(), &Request{
		Adapter:   reservationTestAdapter{},
		Hops:      h.hops,
		Lifecycle: lifecycle.NewContext("req-success", "pipeline", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.store.snapshotCommits()) != 0 {
		t.Fatalf("commits before Body.Close = %d, want 0", len(h.store.snapshotCommits()))
	}

	_, _ = io.Copy(io.Discard, res.Body)
	if err := res.Body.Close(); err != nil {
		t.Fatalf("Body.Close: %v", err)
	}
	waitForCommits(t, h.store, 2)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("post-flight hook did not run")
	}

	commits := h.store.snapshotCommits()
	if len(commits) != 2 {
		t.Fatalf("commit count after post-flight = %d, want 2 (inbound + key exactly once)", len(commits))
	}
	for _, commit := range commits {
		if commit.cancelled {
			t.Fatalf("success commit %q used Cancelled=true, want metered post-flight commit", commit.guardKey)
		}
	}
}

func TestClassifyCallErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       error
		wantRetry bool
		wantKind  keypool.FailureKind
		checkKind bool
	}{
		{
			name:      "caller cancellation does not retry or classify a key failure",
			err:       context.Canceled,
			wantRetry: false,
		},
		{
			name:      "caller deadline does not retry or classify a key failure",
			err:       context.DeadlineExceeded,
			wantRetry: false,
		},
		{
			name:      "dial failure is host-unreachable and retryable",
			err:       &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
			wantRetry: true,
			wantKind:  keypool.FailureUpstreamUnreachable,
			checkKind: true,
		},
		{
			name:      "generic call error is retryable network failure",
			err:       errors.New("read failed"),
			wantRetry: true,
			wantKind:  keypool.FailureNetwork,
			checkKind: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			retry, kind, retryAfter := classify(reservationTestAdapter{}, nil, tc.err)
			if retry != tc.wantRetry {
				t.Fatalf("retry = %v, want %v", retry, tc.wantRetry)
			}
			if tc.checkKind && kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", kind, tc.wantKind)
			}
			if retryAfter != 0 {
				t.Fatalf("retryAfter = %v, want 0", retryAfter)
			}
		})
	}
}
