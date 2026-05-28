package secret

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fakeRefresher struct {
	value   string
	changed bool
	err     error

	mu     sync.Mutex
	calls  int
	called chan struct{}
}

func (f *fakeRefresher) Refresh(_ context.Context, _ string) (string, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	return f.value, f.changed, f.err
}

func TestAgent_NonAuth(t *testing.T) {
	a := NewAgent(&fakeRefresher{}, time.Second, discard())
	if v, _ := a.OnFailure(context.Background(), "k", keypool.FailureServerError, true); v != pipeline.VerdictNext {
		t.Fatalf("non-auth + more candidates: want Next, got %v", v)
	}
	if v, _ := a.OnFailure(context.Background(), "k", keypool.FailureServerError, false); v != pipeline.VerdictFail {
		t.Fatalf("non-auth + last: want Fail, got %v", v)
	}
}

func TestAgent_AuthMoreCandidates_NextAndHealsAsync(t *testing.T) {
	f := &fakeRefresher{value: "new", changed: true, called: make(chan struct{}, 1)}
	a := NewAgent(f, time.Second, discard())

	v, fresh := a.OnFailure(context.Background(), "k", keypool.FailureAuth, true)
	if v != pipeline.VerdictNext || fresh != "" {
		t.Fatalf("auth + more candidates: want Next/\"\", got %v/%q", v, fresh)
	}
	select {
	case <-f.called: // background heal fired
	case <-time.After(2 * time.Second):
		t.Fatal("expected background heal to run")
	}
}

func TestAgent_AuthLast_RotatedRetries(t *testing.T) {
	a := NewAgent(&fakeRefresher{value: "rotated", changed: true}, time.Second, discard())
	v, fresh := a.OnFailure(context.Background(), "k", keypool.FailureAuth, false)
	if v != pipeline.VerdictRetry || fresh != "rotated" {
		t.Fatalf("auth + last + rotated: want Retry/rotated, got %v/%q", v, fresh)
	}
}

func TestAgent_AuthLast_UnchangedFails(t *testing.T) {
	a := NewAgent(&fakeRefresher{value: "same", changed: false}, time.Second, discard())
	if v, _ := a.OnFailure(context.Background(), "k", keypool.FailureAuth, false); v != pipeline.VerdictFail {
		t.Fatalf("auth + last + unchanged: want Fail, got %v", v)
	}
}

func TestAgent_AuthLast_RefreshErrorFails(t *testing.T) {
	a := NewAgent(&fakeRefresher{err: errors.New("boom")}, time.Second, discard())
	if v, _ := a.OnFailure(context.Background(), "k", keypool.FailureAuth, false); v != pipeline.VerdictFail {
		t.Fatalf("auth + last + refresh error: want Fail, got %v", v)
	}
}

type blockingRefresher struct{ release chan struct{} }

func (b *blockingRefresher) Refresh(_ context.Context, _ string) (string, bool, error) {
	<-b.release
	return "", false, nil
}

func TestAgent_AuthLast_CtxCancelledFails(t *testing.T) {
	block := make(chan struct{})
	a := NewAgent(&blockingRefresher{release: block}, 5*time.Second, discard())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller gave up before the (blocked) refresh completes

	if v, _ := a.OnFailure(ctx, "k", keypool.FailureAuth, false); v != pipeline.VerdictFail {
		t.Fatalf("cancelled ctx: want Fail, got %v", v)
	}
	close(block) // let the detached heal goroutine finish
}

func TestAgent_SingleFlight(t *testing.T) {
	// Concurrent last-candidate auth failures for the same key should share
	// one refresh.
	f := &fakeRefresher{value: "rotated", changed: true}
	a := NewAgent(f, time.Second, discard())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.OnFailure(context.Background(), "samekey", keypool.FailureAuth, false)
		}()
	}
	wg.Wait()

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == 0 || f.calls >= 20 {
		t.Fatalf("single-flight: expected far fewer than 20 refreshes, got %d", f.calls)
	}
}
