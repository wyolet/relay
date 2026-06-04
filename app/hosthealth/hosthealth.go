// Package hosthealth records and reads per-host runtime reachability — the
// observed-state counterpart to host.Spec. The data plane writes an outcome
// after each request (reachable on any HTTP response, unreachable after dial
// failures); the control plane reads it to overlay host.Status for the UI.
//
// State lives in pkg/kv under "host_health:{host:<id>}" with a TTL, so an
// idle host's record lapses to "unknown" rather than reporting stale health.
// This is observational only today — routing does not gate on it (a bg prober
// + fail-fast is the planned follow-up). All writes happen in the pipeline's
// detached post-flight goroutines, never on the request latency path.
//
// Expected kv ops: Reachable = 1 Set; Unreachable = 1 Get + 1 Set; Read = 1 Get.
package hosthealth

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/pkg/kv"
)

// defaultTTL bounds how long a health observation survives without refresh.
// Traffic refreshes it; once it lapses, Read reports no record → "unknown".
const defaultTTL = time.Hour

// maxErrLen caps the stored dial-error excerpt.
const maxErrLen = 256

// store is the narrow kv surface this recorder needs.
type store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

// Recorder persists host reachability to kv. Construct once at boot and share
// between the data plane (writes) and the control plane (Read).
type Recorder struct {
	state store
	clock func() time.Time
	ttl   time.Duration
}

// New constructs a Recorder. clock may be nil (defaults to time.Now).
func New(s kv.Store, clock func() time.Time) *Recorder {
	if clock == nil {
		clock = time.Now
	}
	return &Recorder{state: s, clock: clock, ttl: defaultTTL}
}

// Reachable records that the host's upstream answered (any HTTP response —
// reachability, not success). Single unconditional write to stay cheap on the
// (async) success path; LastTransition doubles as last-seen-healthy.
func (r *Recorder) Reachable(ctx context.Context, hostID string) {
	if r == nil || r.state == nil || hostID == "" {
		return
	}
	now := r.clock()
	r.write(ctx, hostID, host.Status{
		Health:         host.HealthHealthy,
		LastTransition: now,
		LastSuccess:    now,
	})
}

// Unreachable records a dial failure, carrying the error excerpt and bumping
// the consecutive-failure counter off the prior record.
func (r *Recorder) Unreachable(ctx context.Context, hostID, errMsg string) {
	if r == nil || r.state == nil || hostID == "" {
		return
	}
	now := r.clock()
	prev, _ := r.Read(ctx, hostID)
	if len(errMsg) > maxErrLen {
		errMsg = errMsg[:maxErrLen]
	}
	r.write(ctx, hostID, host.Status{
		Health:              host.HealthUnreachable,
		LastError:           errMsg,
		ConsecutiveFailures: prev.ConsecutiveFailures + 1,
		LastTransition:      now,
		LastSuccess:         prev.LastSuccess,
	})
}

// Read returns the stored Status and whether a record exists. A missing or
// undecodable record yields a zero Status (Health == HealthUnknown), found=false.
func (r *Recorder) Read(ctx context.Context, hostID string) (host.Status, bool) {
	if r == nil || r.state == nil || hostID == "" {
		return host.Status{}, false
	}
	b, err := r.state.Get(ctx, healthKey(hostID))
	if err != nil || len(b) == 0 {
		return host.Status{}, false
	}
	var st host.Status
	if json.Unmarshal(b, &st) != nil {
		return host.Status{}, false
	}
	return st, true
}

func (r *Recorder) write(ctx context.Context, hostID string, st host.Status) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = r.state.Set(ctx, healthKey(hostID), b, r.ttl)
}
