package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// serverErrorAdapter answers every call with a retryable 503.
type serverErrorAdapter struct{ reservationTestAdapter }

func (serverErrorAdapter) Call(context.Context, string, *string, string, []byte, http.Header, string, bool, bool) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

func (serverErrorAdapter) Retryable(*http.Response) (bool, keypool.FailureKind, time.Duration) {
	return true, keypool.FailureServerError, 0
}

// A request whose every key fails writes its settles and key failures on the post-flight store only, and the caller's hold settles before the error is returned.
func TestRunFailureWritesUsePostFlightStore(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	shared, post := kvtest.NewRecording(mem), kvtest.NewRecording(mem)

	inbound := concurrencyRateLimit("rl-inbound", "inbound")
	upstream := concurrencyRateLimit("rl-upstream", "upstream")
	acc := accountPlan("acct-1")
	hop := buildHop(keypool.KeySelectionPrioritized, callerCounter(inbound),
		testSlot{key: testKey("key-a"), acc: acc, upstream: accountCounters("acct-1", upstream)},
		testSlot{key: testKey("key-b"), acc: acc, upstream: accountCounters("acct-1", upstream)})
	p := &Pipeline{
		Limiter:      lua.New(pkgratelimit.New(shared, slog.Default(), nil)).WithPostFlight(pkgratelimit.New(post, slog.Default(), nil)),
		Selector:     keypool.New(shared, slog.Default(), nil, nil),
		PostSelector: keypool.New(post, slog.Default(), nil, nil),
		Logger:       slog.Default(),
	}

	_, err := p.Run(context.Background(), &Request{Adapter: serverErrorAdapter{}, Hops: []plan.RouteHop{hop}})
	var failure *UpstreamFailureError
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("Run err = %v, want the upstream 503", err)
	}
	callerSettled := false
	for _, c := range post.Named("limit.commit") {
		callerSettled = callerSettled || strings.HasPrefix(c.Keys[0], "limit:{team:t}:")
	}
	if !callerSettled {
		t.Fatal("the caller's hold was not settled before Run returned")
	}

	// Two attempts (an account settle and a failure write each) and the caller's settle.
	deadline := time.Now().Add(5 * time.Second)
	for len(post.Named("limit.commit")) < 3 || len(post.Named("keypool.record_failure")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("post-flight scripts: %d commits, %d failure writes; want 3, 2", len(post.Named("limit.commit")), len(post.Named("keypool.record_failure")))
		}
		time.Sleep(time.Millisecond)
	}
	if n, r := len(shared.Named("limit.commit")), len(shared.Named("keypool.record_failure")); n != 0 || r != 0 {
		t.Fatalf("shared store ran %d commits and %d failure writes, want none", n, r)
	}
}
