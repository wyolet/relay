package inference

import "sync"

// catalogCacheMax bounds the scopes held at once; past it the whole map is dropped and refilled on demand, which costs one render per scope.
const catalogCacheMax = 1024

// catalogEntry is one scope's rendered /v1/catalog document. gen is the snapshot generation it was built from: the invalidation signal only, never the validator, because generations restart at 1 in every process.
type catalogEntry struct {
	gen     uint64
	version string
	etag    string
	json    []byte
	gz      []byte
}

// catalogCache holds rendered documents per scope: a policy id, or "policyless|<userID>".
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]*catalogEntry
}

func newCatalogCache() *catalogCache {
	return &catalogCache{entries: map[string]*catalogEntry{}}
}

// get returns scope's entry while it was built from snapshot gen and seeded catalog version.
func (c *catalogCache) get(scope string, gen uint64, version string) (*catalogEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[scope]
	if !ok || e.gen != gen || e.version != version {
		return nil, false
	}
	return e, true
}

func (c *catalogCache) put(scope string, e *catalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[scope]; !ok && len(c.entries) >= catalogCacheMax {
		c.entries = map[string]*catalogEntry{}
	}
	c.entries[scope] = e
}
