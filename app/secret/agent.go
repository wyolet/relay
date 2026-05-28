// agent.go implements pipeline.KeyAgent: the out-of-band handler for failed
// upstream keys. The request loop calls OnFailure and obeys the verdict; all
// the secret-shaped concerns — re-resolution, single-flight de-duplication,
// park-and-wait, healing the snapshot, alerting — live here, off the request's
// import graph. The pipeline imports only the KeyAgent interface, never this.
package secret

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
)

// Refresher re-resolves a host key's secret from its backend and persists the
// fresh value (so later requests see it), returning the value and whether it
// changed. Implemented at composition over the hostkey store + catalog.
type Refresher interface {
	Refresh(ctx context.Context, keyID string) (value string, changed bool, err error)
}

// Agent implements pipeline.KeyAgent. On an auth failure it single-flights a
// secret re-resolve: when other candidates remain it heals in the background
// and tells the request to fail over (never waits); when this was the last
// candidate it parks on the re-resolve and retries the same key if the secret
// had rotated.
type Agent struct {
	refresher Refresher
	timeout   time.Duration
	log       *slog.Logger
	sf        singleflight.Group
}

// NewAgent builds the agent. timeout bounds a single re-resolve (and therefore
// how long a last-candidate request can park). Defaults: 5s, slog.Default().
func NewAgent(r Refresher, timeout time.Duration, log *slog.Logger) *Agent {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Agent{refresher: r, timeout: timeout, log: log}
}

type healResult struct {
	value   string
	changed bool
}

// OnFailure implements pipeline.KeyAgent.
func (a *Agent) OnFailure(ctx context.Context, keyID string, kind keypool.FailureKind, moreCandidates bool) (pipeline.Verdict, string) {
	// Only an auth failure implies a possibly-rotated secret; everything else
	// is a plain failover (or stop when this was the last candidate).
	if kind != keypool.FailureAuth {
		if moreCandidates {
			return pipeline.VerdictNext, ""
		}
		return pipeline.VerdictFail, ""
	}

	if moreCandidates {
		// Optimistic: fail over now, heal in the background for next time.
		a.sf.DoChan(keyID, a.heal(keyID))
		return pipeline.VerdictNext, ""
	}

	// Last candidate: nothing to fail over to. Park on the single-flighted
	// re-resolve; retry the same key if it rotated. Bounded by the request ctx
	// (caller gave up) and the heal's own timeout (the DoChan result arrives).
	select {
	case r := <-a.sf.DoChan(keyID, a.heal(keyID)):
		if r.Err != nil {
			return pipeline.VerdictFail, ""
		}
		res, _ := r.Val.(healResult)
		if res.changed {
			return pipeline.VerdictRetry, res.value
		}
		return pipeline.VerdictFail, ""
	case <-ctx.Done():
		return pipeline.VerdictFail, ""
	}
}

// heal is the single-flight function: re-resolve + persist on a DETACHED
// context so a fire-and-forget heal completes even after the triggering
// request returns. Logs the "still invalid after refresh" case (revoked key,
// not rotated) so operators can act on it.
func (a *Agent) heal(keyID string) func() (any, error) {
	return func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
		defer cancel()
		value, changed, err := a.refresher.Refresh(ctx, keyID)
		switch {
		case err != nil:
			a.log.Warn("keyagent: secret refresh failed", "key_id", keyID, "err", err)
		case !changed:
			a.log.Warn("keyagent: key still invalid after refresh (revoked, not rotated?)", "key_id", keyID)
		default:
			a.log.Info("keyagent: key rotated upstream, healed", "key_id", keyID)
		}
		return healResult{value: value, changed: changed}, err
	}
}
