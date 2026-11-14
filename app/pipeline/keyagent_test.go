package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/kv"
)

// asked is one failure a key agent was consulted on.
type asked struct {
	KeyID string
	Kind  keypool.FailureKind
	More  bool
}

// scriptedAgent answers each failure with its next verdict (VerdictNext once they run out), handing out fresh as the healed secret.
type scriptedAgent struct {
	verdicts []keypool.Verdict
	fresh    string
	asked    []asked
}

func (a *scriptedAgent) OnFailure(_ context.Context, keyID string, kind keypool.FailureKind, more bool) (keypool.Verdict, string) {
	a.asked = append(a.asked, asked{keyID, kind, more})
	if len(a.verdicts) == 0 {
		return keypool.VerdictNext, ""
	}
	v := a.verdicts[0]
	a.verdicts = a.verdicts[1:]
	return v, a.fresh
}

// The request loop obeys the agent: fail over, retry the same credential with the healed secret, or stop. The breaker is written whenever the credential is given up on and never on a heal-retry; a failure that is not the credential's never reaches the agent.
func TestRun_ObeysTheKeyAgent(t *testing.T) {
	t.Parallel()
	authOpen := &breaker{State: keypool.CircuitOpen, Reason: keypool.ReasonUpstreamAuthFailed, Step: 1}
	for _, tc := range []struct {
		name       string
		keys       []string // credential values; the hashes are h1, h2
		status     map[string]int
		verdicts   []keypool.Verdict
		wantCalls  []string
		wantAsked  []asked
		wantStatus int // 0: Run fails with the last upstream status
		wantKey    string
		wantH1     *breaker
	}{
		{name: "next: fails over without waiting", keys: []string{"stale", "good"}, status: map[string]int{"stale": 401},
			verdicts:  []keypool.Verdict{keypool.VerdictNext},
			wantCalls: []string{"stale", "good"}, wantAsked: []asked{{"key-h1", keypool.FailureAuth, true}},
			wantStatus: 200, wantKey: "h2", wantH1: authOpen},
		{name: "retry: the last credential is called again with the healed secret", keys: []string{"stale"}, status: map[string]int{"stale": 401},
			verdicts:  []keypool.Verdict{keypool.VerdictRetry},
			wantCalls: []string{"stale", "healed"}, wantAsked: []asked{{"key-h1", keypool.FailureAuth, false}},
			wantStatus: 200, wantKey: "h1"},
		{name: "retry then fail: a healed secret that is rejected too stops the request", keys: []string{"stale"}, status: map[string]int{"stale": 401, "healed": 401},
			verdicts:  []keypool.Verdict{keypool.VerdictRetry, keypool.VerdictFail},
			wantCalls: []string{"stale", "healed"}, wantAsked: []asked{{"key-h1", keypool.FailureAuth, false}, {"key-h1", keypool.FailureAuth, false}},
			wantH1: authOpen},
		{name: "fail: stops without trying the next credential", keys: []string{"stale", "good"}, status: map[string]int{"stale": 401},
			verdicts:  []keypool.Verdict{keypool.VerdictFail},
			wantCalls: []string{"stale"}, wantAsked: []asked{{"key-h1", keypool.FailureAuth, true}},
			wantH1: authOpen},
		{name: "a server error is the agent's to judge too", keys: []string{"stale", "good"}, status: map[string]int{"stale": 503},
			wantCalls: []string{"stale", "good"}, wantAsked: []asked{{"key-h1", keypool.FailureServerError, true}},
			wantStatus: 200, wantKey: "h2", wantH1: &breaker{Failures: 1}},
		{name: "a 400 is not the credential's", keys: []string{"stale", "good"}, status: map[string]int{"stale": 400},
			wantCalls: []string{"stale"}, wantStatus: 400, wantKey: "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := kv.NewMem()
			t.Cleanup(func() { _ = mem.Close() })
			agent := &scriptedAgent{verdicts: tc.verdicts, fresh: "healed"}
			p := pipelineOver(mem)
			p.KeyAgent = agent
			adp := newKeyedAdapter()
			adp.status = tc.status
			keys := []*hostkey.HostKey{makeKey("h1", tc.keys[0])}
			if len(tc.keys) > 1 {
				keys = append(keys, makeKey("h2", tc.keys[1]))
			}
			res, err := p.Run(context.Background(), &pipeline.Request{Adapter: adp, Hops: hopOf(keys)})
			if d := cmp.Diff(tc.wantCalls, adp.calls); d != "" {
				t.Errorf("secrets called (-want +got):\n%s", d)
			}
			if d := cmp.Diff(tc.wantAsked, agent.asked); d != "" {
				t.Errorf("agent consulted (-want +got):\n%s", d)
			}
			if tc.wantStatus == 0 {
				var upstream *pipeline.UpstreamFailureError
				if res != nil || !errors.As(err, &upstream) || upstream.Status != 401 || !errors.Is(err, pipeline.ErrAllKeysExhausted) {
					t.Fatalf("Run = %v, %v; want the upstream's 401 as an exhausted-keys error", res, err)
				}
			} else {
				if err != nil || res.Status != tc.wantStatus || res.KeyHash != tc.wantKey {
					t.Fatalf("Run = %+v, %v; want %d served by %s", res, err, tc.wantStatus, tc.wantKey)
				}
				drainResult(t, res)
			}
			wantBreaker(t, p.Selector, "h1", tc.wantH1)
			wantBreaker(t, p.Selector, "h2", nil)
		})
	}
}
