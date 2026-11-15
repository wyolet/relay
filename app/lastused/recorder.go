// Package lastused records when each relay key and host key last served a
// request, independent of usage logging. A lifecycle Collector notes the ids
// of every finished request in memory, to the minute; a Flusher writes them
// to Postgres about once a minute in one statement per kind. The tables carry
// no catalog trigger, so the writes rebuild no pod's snapshot.
//
// Out of scope: inference tokens, which have no stored row to attach a time
// to, and the request path itself, which never reads these times.
package lastused

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// DefaultMaxEntries bounds the ids of each kind noted between two flushes.
const DefaultMaxEntries = 1 << 16

// credentialKey is lifecycle.Context.CredentialKind for a relay key (inference.CredentialKey), whose CredentialID is the key's row id. Spelled here so an observer imports no request runner.
const credentialKey = "key"

// usedSet maps each id to the unix minute it last served.
type usedSet struct {
	keys     map[string]int64
	hostKeys map[string]int64
}

func newUsedSet() *usedSet {
	return &usedSet{keys: map[string]int64{}, hostKeys: map[string]int64{}}
}

// Recorder is the lifecycle Collector that notes which credentials served.
// Collect holds the lock for two map writes; Drain swaps in a second set, so
// a flush never makes a request wait on its iteration.
type Recorder struct {
	mu  sync.Mutex
	cur *usedSet

	// drainMu serializes Drain; spare is touched only under it.
	drainMu sync.Mutex
	spare   *usedSet

	max  int
	now  func() time.Time
	lost prometheus.Counter
}

// NewRecorder returns a Recorder holding at most maxEntries ids per kind
// between flushes; ids beyond it are dropped and counted in
// relay_records_lost_total{kind="last_used"}. maxEntries <= 0 takes
// DefaultMaxEntries.
func NewRecorder(maxEntries int) *Recorder {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Recorder{
		cur:   newUsedSet(),
		spare: newUsedSet(),
		max:   maxEntries,
		now:   time.Now,
		lost:  metrics.RecordsLost.WithLabelValues("last_used"),
	}
}

// Collect notes the request's relay key, and the host key of its last
// upstream attempt. A request refused before any upstream call names no host
// key; a token names no relay key.
func (r *Recorder) Collect(lc *lifecycle.Context) {
	if lc == nil {
		return
	}
	var keyID string
	if lc.CredentialKind == credentialKey {
		keyID = lc.CredentialID
	}
	hostKeyID := lc.HostKeyRowID
	if strings.HasPrefix(hostKeyID, hostkey.AnonIDPrefix) {
		hostKeyID = ""
	}
	if keyID == "" && hostKeyID == "" {
		return
	}
	minute := r.now().Unix() / 60
	r.mu.Lock()
	r.note(r.cur.keys, keyID, minute)
	r.note(r.cur.hostKeys, hostKeyID, minute)
	r.mu.Unlock()
}

// note keeps the later of minute and what m holds for id. Caller holds mu.
func (r *Recorder) note(m map[string]int64, id string, minute int64) {
	if id == "" {
		return
	}
	if cur, ok := m[id]; ok {
		if minute > cur {
			m[id] = minute
		}
		return
	}
	if len(m) >= r.max {
		r.lost.Inc()
		return
	}
	m[id] = minute
}

// Drain hands fn everything noted since the last Drain, then empties it.
// Collect notes into the other set meanwhile. fn must not keep the maps.
func (r *Recorder) Drain(fn func(keys, hostKeys map[string]int64)) {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	r.mu.Lock()
	full := r.cur
	r.cur = r.spare
	r.mu.Unlock()
	fn(full.keys, full.hostKeys)
	// clear keeps the maps' storage, so the next interval allocates nothing for ids seen before.
	clear(full.keys)
	clear(full.hostKeys)
	r.spare = full
}

// restore notes entries again, after a flush that failed to write them.
func (r *Recorder) restore(keys, hostKeys []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range keys {
		r.note(r.cur.keys, e.ID, e.At.Unix()/60)
	}
	for _, e := range hostKeys {
		r.note(r.cur.hostKeys, e.ID, e.At.Unix()/60)
	}
}
