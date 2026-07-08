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
// Expected kv ops: Reachable = 1 Set, debounced to ≤1 per host per interval
// (most calls under load are no kv op); Unreachable = 1 Get + 1 Set; Read = 1
// Get; ReadAll = 1 Range (SCAN + batched MGET on Redis).
package hosthealth

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/pkg/kv"
)

// defaultTTL bounds how long a health observation survives without refresh.
// Traffic refreshes it; once it lapses, Read reports no record → "unknown".
const defaultTTL = time.Hour

// maxErrLen caps the stored dial-error excerpt.
const maxErrLen = 256

// defaultReachableDebounce is the minimum spacing between successful-path
// Reachable writes for a single host. At 2k rps to one host the reachable
// signal is otherwise 2k identical SETs/s; a healthy host only needs its
// last-seen refreshed roughly once per interval. Failure-path (Unreachable)
// writes are never debounced.
//
// TODO(config): promote to RELAY_HOSTHEALTH_REACHABLE_DEBOUNCE. Hardcoded here
// to avoid colliding with a concurrent internal/config change; see WS3 notes.
const defaultReachableDebounce = time.Second

// store is the narrow kv surface this recorder needs.
type store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Range(ctx context.Context, prefix string) ([]kv.Entry, error)
}

// Recorder persists host reachability to kv. Construct once at boot and share
// between the data plane (writes) and the control plane (Read).
type Recorder struct {
	state    store
	clock    func() time.Time
	ttl      time.Duration
	debounce time.Duration

	mu            sync.Mutex
	lastReachable map[string]time.Time // hostID → last successful-path write
}

// New constructs a Recorder. clock may be nil (defaults to time.Now).
func New(s kv.Store, clock func() time.Time) *Recorder {
	if clock == nil {
		clock = time.Now
	}
	return &Recorder{
		state:         s,
		clock:         clock,
		ttl:           defaultTTL,
		debounce:      defaultReachableDebounce,
		lastReachable: make(map[string]time.Time),
	}
}

// Reachable records that the host's upstream answered (any HTTP response —
// reachability, not success). LastTransition doubles as last-seen-healthy.
//
// Writes are debounced per host: under a firehose of successes to one host we
// refresh the record at most once per debounce interval instead of once per
// request (2k rps → 2k identical SETs/s otherwise). The first write for a host
// and the first write after any Unreachable are never debounced, so recovery
// is reflected immediately.
func (r *Recorder) Reachable(ctx context.Context, hostID string) {
	if r == nil || r.state == nil || hostID == "" {
		return
	}
	now := r.clock()
	if !r.allowReachableWrite(hostID, now) {
		return
	}
	r.write(ctx, hostID, host.Status{
		Health:         host.HealthHealthy,
		LastTransition: now,
		LastSuccess:    now,
	})
}

// allowReachableWrite reports whether a Reachable write for hostID should
// proceed now, recording the timestamp when it grants one.
func (r *Recorder) allowReachableWrite(hostID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastReachable[hostID]; ok && now.Sub(last) < r.debounce {
		return false
	}
	r.lastReachable[hostID] = now
	return true
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
	// Clear the debounce gate so the next Reachable (recovery) writes at once
	// rather than waiting out the interval on a stale healthy timestamp.
	r.mu.Lock()
	delete(r.lastReachable, hostID)
	r.mu.Unlock()
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

// ReadAll returns every stored host Status keyed by host id in one kv Range —
// the batch counterpart to Read for list endpoints, avoiding a Get per host.
// Undecodable records are skipped, mirroring Read's lenient contract.
func (r *Recorder) ReadAll(ctx context.Context) map[string]host.Status {
	if r == nil || r.state == nil {
		return nil
	}
	entries, err := r.state.Range(ctx, "host_health:")
	if err != nil || len(entries) == 0 {
		return nil
	}
	out := make(map[string]host.Status, len(entries))
	for _, e := range entries {
		id := hostIDFromKey(e.Key)
		if id == "" {
			continue
		}
		var st host.Status
		if json.Unmarshal(e.Value, &st) != nil {
			continue
		}
		out[id] = st
	}
	return out
}

func (r *Recorder) write(ctx context.Context, hostID string, st host.Status) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = r.state.Set(ctx, healthKey(hostID), b, r.ttl)
}
