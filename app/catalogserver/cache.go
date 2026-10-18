package catalogserver

import (
	"container/list"
	"sync"
)

type fileKey struct {
	tag   string
	asset asset
}

type cacheEntry struct {
	key  fileKey
	data []byte
}

// fileCache is an LRU over file bytes within a byte budget. Pinned keys
// count against the budget but are never evicted.
type fileCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	order  *list.List
	items  map[fileKey]*list.Element
	pinned map[fileKey]bool
}

func newFileCache(budget int64) *fileCache {
	return &fileCache{
		budget: budget,
		order:  list.New(),
		items:  map[fileKey]*list.Element{},
		pinned: map[fileKey]bool{},
	}
}

func (c *fileCache) get(k fileKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheEntry).data, true
}

// put drops an unpinned file larger than the whole budget instead of
// evicting everything for it.
func (c *fileCache) put(k fileKey, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	if !c.pinned[k] && int64(len(data)) > c.budget {
		return
	}
	c.items[k] = c.order.PushFront(&cacheEntry{key: k, data: data})
	c.used += int64(len(data))
	c.evict()
}

// setPinned replaces the pinned set; previously pinned files become
// evictable.
func (c *fileCache) setPinned(keys []fileKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned = make(map[fileKey]bool, len(keys))
	for _, k := range keys {
		c.pinned[k] = true
	}
	c.evict()
}

func (c *fileCache) evict() {
	for e := c.order.Back(); e != nil && c.used > c.budget; {
		prev := e.Prev()
		ent := e.Value.(*cacheEntry)
		if !c.pinned[ent.key] {
			c.order.Remove(e)
			delete(c.items, ent.key)
			c.used -= int64(len(ent.data))
		}
		e = prev
	}
}

func (c *fileCache) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}
