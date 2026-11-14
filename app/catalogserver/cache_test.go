package catalogserver

import "testing"

func TestFileCache_EvictsUnpinnedOnly(t *testing.T) {
	c := newFileCache(10)
	held := func(k fileKey) bool {
		_, ok := c.get(k)
		return ok
	}
	latest := fileKey{"v0.1.2", assetCatalog}
	c.setPinned([]fileKey{latest})
	c.put(latest, make([]byte, 6))
	c.put(fileKey{"v0.1.0", assetCatalog}, make([]byte, 3))
	c.put(fileKey{"v0.1.1", assetCatalog}, make([]byte, 3))
	if held(fileKey{"v0.1.0", assetCatalog}) || !held(fileKey{"v0.1.1", assetCatalog}) || !held(latest) || c.bytes() != 9 {
		t.Fatalf("over budget: want only the least recently used unpinned file evicted, %d bytes held", c.bytes())
	}
	c.put(fileKey{"v0.1.1", assetCatalog}, make([]byte, 100))
	if c.bytes() != 9 {
		t.Fatalf("a second put of a held key changed the cache to %d bytes", c.bytes())
	}
	c.put(fileKey{"v0.0.9", assetData}, make([]byte, 11))
	if held(fileKey{"v0.0.9", assetData}) || c.bytes() != 9 {
		t.Fatal("unpinned file larger than the budget cached")
	}
	c.setPinned(nil)
	c.put(fileKey{"v0.1.3", assetCatalog}, make([]byte, 5))
	if held(latest) || !held(fileKey{"v0.1.3", assetCatalog}) || c.bytes() != 8 {
		t.Fatalf("after unpinning: want the former latest evicted, %d bytes held", c.bytes())
	}
}
