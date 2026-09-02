// token_cache.go holds the verified-claims cache the token path reads
// before paying for an Ed25519 verification. Deliberately not an LRU: a
// token's key is its own digest and every entry has the same short life, so
// two generations bound the size without a recency list on the hot path.
package inference

import (
	"sync"
	"sync/atomic"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/crypto"
)

// cacheShards must be a power of two — the digest's first byte is masked to
// pick a shard.
const cacheShards = 16

// cacheEntry is one verified bearer. claims and exp are fixed at insert;
// subjects is recomputed whenever the snapshot they were derived from is
// replaced, so a membership change is not held until the token expires.
type cacheEntry struct {
	claims crypto.TokenClaims
	exp    time.Time

	mu       sync.Mutex
	snap     *appcatalog.Snapshot
	subjects []string
}

func (e *cacheEntry) subjectsFor(snap *appcatalog.Snapshot) ([]string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snap != snap || e.subjects == nil {
		return nil, false
	}
	return e.subjects, true
}

func (e *cacheEntry) setSubjects(snap *appcatalog.Snapshot, subs []string) {
	e.mu.Lock()
	e.snap, e.subjects = snap, subs
	e.mu.Unlock()
}

// claimsCache is a sharded, bounded map. Each shard keeps two generations:
// when the live one fills, it becomes the backup and a fresh map takes over,
// so the shard holds at most twice its share and eviction costs one
// assignment.
type claimsCache struct {
	// size is the whole-cache bound; 0 disables the cache entirely.
	size   atomic.Int64
	shards [cacheShards]cacheShard
}

type cacheShard struct {
	mu             sync.RWMutex
	live, previous map[string]*cacheEntry
}

func (c *claimsCache) resize(n int) {
	if n <= 0 {
		// Explicitly off: distinct from the unset zero value, which still
		// gets the default bound so the cache works before the settings
		// section is first delivered.
		c.size.Store(-1)
		c.clear()
		return
	}
	c.size.Store(int64(n))
}

func (c *claimsCache) clear() {
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		s.live, s.previous = nil, nil
		s.mu.Unlock()
	}
}

// perShard is the live generation's cap. Defaults apply until the settings
// section is delivered, so the cache is useful from the first request.
func (c *claimsCache) perShard() int {
	n := c.size.Load()
	switch {
	case n < 0:
		return 0
	case n == 0:
		n = settings.DefaultVerifyCacheSize
	}
	per := int(n) / cacheShards / 2
	if per < 1 {
		per = 1
	}
	return per
}

func (c *claimsCache) get(digest string, now time.Time) *cacheEntry {
	if c.perShard() == 0 {
		return nil
	}
	s := &c.shards[shardOf(digest)]
	s.mu.RLock()
	ent, ok := s.live[digest]
	if !ok {
		ent, ok = s.previous[digest]
	}
	s.mu.RUnlock()
	if !ok || !now.Before(ent.exp) {
		return nil
	}
	return ent
}

func (c *claimsCache) put(digest string, ent *cacheEntry) {
	per := c.perShard()
	if per == 0 {
		return
	}
	s := &c.shards[shardOf(digest)]
	s.mu.Lock()
	if s.live == nil {
		s.live = make(map[string]*cacheEntry, per)
	}
	if len(s.live) >= per {
		s.previous, s.live = s.live, make(map[string]*cacheEntry, per)
	}
	s.live[digest] = ent
	s.mu.Unlock()
}

// shardOf reads the digest's leading hex byte; a sha256 digest is uniform,
// so this spreads evenly without hashing the string again.
func shardOf(digest string) int {
	if digest == "" {
		return 0
	}
	return int(digest[0]) & (cacheShards - 1)
}
