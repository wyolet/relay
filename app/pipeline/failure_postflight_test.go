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

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
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

// A request whose every key fails writes its refunds and key failures on the post-flight store only: each failover's refund and failure write share a round trip (detached), and the inbound reservation settles before the error is returned.
func TestRunFailureWritesUsePostFlightStore(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	shared, post := kvtest.NewRecording(mem), kvtest.NewRecording(mem)

	inbound := concurrencyRateLimit("rl-inbound", "inbound")
	upstream := concurrencyRateLimit("rl-upstream", "upstream")
	tier := &policy.Policy{Meta: meta.Metadata{ID: "pol-tier", Name: "tier"}, Spec: policy.Spec{RateLimitID: upstream.Meta.ID}}
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: "pol-caller", Name: "caller"},
		Spec: policy.Spec{RateLimitID: inbound.Meta.ID, KeySelection: policy.KeySelectionPrioritized},
	}
	snap := &reservationTestSnapshot{
		policies:   map[string]*policy.Policy{tier.Meta.ID: tier},
		rateLimits: map[string]*appratelimit.RateLimit{inbound.Meta.ID: inbound, upstream.Meta.ID: upstream},
	}
	var keys []*hostkey.HostKey
	for _, id := range []string{"key-a", "key-b"} {
		keys = append(keys, &hostkey.HostKey{
			Meta: meta.Metadata{ID: id, Name: id}, Spec: hostkey.Spec{PolicyID: tier.Meta.ID},
			Resolved: "sk-" + id, KeyHash: "hash-" + id,
		})
	}
	svc := policy.NewService(snap, keypool.New(shared, slog.Default(), nil, nil), pkgratelimit.New(shared, slog.Default(), nil)).
		WithPostFlight(keypool.New(post, slog.Default(), nil, nil), pkgratelimit.New(post, slog.Default(), nil))
	p := &Pipeline{Policy: svc, Logger: slog.Default()}

	_, err := p.Run(context.Background(), &Request{Adapter: serverErrorAdapter{}, Keys: keys, Policy: caller})
	var failure *UpstreamFailureError
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("Run err = %v, want the upstream 503", err)
	}
	inboundSettled := false
	for _, c := range post.Named("limit.commit") {
		inboundSettled = inboundSettled || strings.HasPrefix(c.Keys[0], "limit:{caller}:")
	}
	if !inboundSettled {
		t.Fatal("the inbound reservation was not settled before Run returned")
	}

	// Two failovers (a refund and a failure write each) and the inbound commit.
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
	if b := post.Batches(); len(b) != 2 || b[0] != 2 || b[1] != 2 {
		t.Fatalf("post-flight batches = %v, want two of 2 (each failover's refund and failure write)", b)
	}
}
