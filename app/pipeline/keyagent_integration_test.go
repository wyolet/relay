package pipeline_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pipeline"
	appsecret "github.com/wyolet/relay/app/secret"
)

// tracingRefresher implements appsecret.Refresher and logs every re-resolve so
// the test output shows exactly when/why the agent re-fetched a secret.
type tracingRefresher struct {
	t       *testing.T
	value   string
	changed bool
	err     error
	called  chan string
}

func (r *tracingRefresher) Refresh(_ context.Context, keyID string) (string, bool, error) {
	r.t.Logf("    [refresher] re-resolve key=%q → value=%q changed=%v err=%v", keyID, r.value, r.changed, r.err)
	if r.called != nil {
		select {
		case r.called <- keyID:
		default:
		}
	}
	return r.value, r.changed, r.err
}

func keyWith(id, hash, resolved string) *hostkey.HostKey {
	return &hostkey.HostKey{
		Meta:     meta.Metadata{ID: id, Name: id},
		Resolved: resolved,
		KeyHash:  hash,
	}
}

// validatesKey builds a fake upstream that 200s only for keys in `good`,
// otherwise 401s — logging every call so the failover/retry sequence is visible.
func validatesKey(t *testing.T, good map[string]bool) *fakeAdapter {
	return &fakeAdapter{
		callFn: func(_ context.Context, _, key string, _ []byte, _ http.Header) (*http.Response, error) {
			if good[key] {
				t.Logf("    [upstream] key=%q → 200 OK", key)
				return okResp("ok"), nil
			}
			t.Logf("    [upstream] key=%q → 401 Unauthorized", key)
			return errResp(401), nil
		},
		retryFn: func(resp *http.Response) (bool, keypool.FailureKind, time.Duration) {
			if resp != nil && resp.StatusCode == 401 {
				return true, keypool.FailureAuth, 0
			}
			return false, 0, 0
		},
	}
}

// Case 1 — failover: key1 is stale (401), key2 is good. The request must rotate
// to key2 WITHOUT waiting; key1's heal fires in the background.
func TestKeyAgent_Failover_DoesNotWait(t *testing.T) {
	t.Log("CASE 1: two candidates, key1 rotated/stale, key2 good → expect failover to key2, async heal of key1")

	key1 := keyWith("key1", "hash1", "old-bad")
	key2 := keyWith("key2", "hash2", "good")
	adp := validatesKey(t, map[string]bool{"good": true})

	healed := make(chan string, 1)
	ref := &tracingRefresher{t: t, value: "rotated", changed: true, called: healed}
	p := &pipeline.Pipeline{
		Policy:   newService(nil),
		Logger:   slog.Default(),
		KeyAgent: appsecret.NewAgent(ref, 2*time.Second, slog.New(slog.DiscardHandler)),
	}

	t.Log("  → Run with keys [key1(old-bad), key2(good)]")
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1, key2},
		Policy:  makePolicy(),
	})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.KeyHash != "hash2" {
		t.Errorf("served by KeyHash=%q, want hash2 (failed over to key2)", res.KeyHash)
	}
	drainResult(t, res)
	if got := adp.callCount.Load(); got != 2 {
		t.Errorf("upstream calls=%d, want 2 (key1 fail + key2 ok)", got)
	}

	t.Log("  → request succeeded via key2; now confirm key1 healed in background")
	select {
	case id := <-healed:
		t.Logf("  ✓ background heal ran for %q (did NOT block the request)", id)
	case <-time.After(2 * time.Second):
		t.Fatal("expected background heal of key1 to run")
	}
}

// Case 2 — last candidate, rotated: only key1, which is stale (401). The agent
// parks on the re-resolve, gets the rotated value, and the request retries the
// SAME key successfully. No second key needed.
func TestKeyAgent_LastCandidate_RotatedWaitsAndRetries(t *testing.T) {
	t.Log("CASE 2: single candidate, key rotated upstream → expect park-on-refresh then retry SAME key with fresh value")

	key1 := keyWith("key1", "hash1", "old-bad")
	// Upstream accepts only the rotated value.
	adp := validatesKey(t, map[string]bool{"rotated-good": true})
	ref := &tracingRefresher{t: t, value: "rotated-good", changed: true}

	p := &pipeline.Pipeline{
		Policy:   newService(nil),
		Logger:   slog.Default(),
		KeyAgent: appsecret.NewAgent(ref, 2*time.Second, slog.New(slog.DiscardHandler)),
	}

	t.Log("  → Run with keys [key1(old-bad)] only")
	res, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1},
		Policy:  makePolicy(),
	})
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	drainResult(t, res)
	if got := adp.callCount.Load(); got != 2 {
		t.Errorf("upstream calls=%d, want 2 (old-bad 401 + rotated-good 200)", got)
	}
	t.Log("  ✓ retried same key with refreshed value and succeeded")
}

// Case 3 — last candidate, NOT rotated: the re-resolve returns the same bad
// value (revoked, not rotated). The agent returns Fail; the request errors
// cleanly instead of looping.
func TestKeyAgent_LastCandidate_StillBadFailsClean(t *testing.T) {
	t.Log("CASE 3: single candidate, key revoked (refresh returns same value) → expect clean error, no loop")

	key1 := keyWith("key1", "hash1", "old-bad")
	adp := validatesKey(t, map[string]bool{}) // nothing is good
	ref := &tracingRefresher{t: t, value: "old-bad", changed: false}

	p := &pipeline.Pipeline{
		Policy:   newService(nil),
		Logger:   slog.Default(),
		KeyAgent: appsecret.NewAgent(ref, 2*time.Second, slog.New(slog.DiscardHandler)),
	}

	t.Log("  → Run with keys [key1(old-bad)] only")
	_, err := p.Run(context.Background(), &pipeline.Request{
		Adapter: adp,
		Keys:    []*hostkey.HostKey{key1},
		Policy:  makePolicy(),
	})
	if err == nil {
		t.Fatal("expected an error (key revoked), got nil")
	}
	var upstream *pipeline.UpstreamFailureError
	if !errors.As(err, &upstream) || upstream.Status != 401 {
		t.Errorf("want UpstreamFailureError(401), got %v", err)
	}
	if got := adp.callCount.Load(); got != 1 {
		t.Errorf("upstream calls=%d, want 1 (no retry on unchanged value)", got)
	}
	t.Logf("  ✓ failed cleanly: %v", err)
}
