// Package cell owns long-lived runtime state (counters, breaker copies,
// rotation state, shared caches) keyed by stable ids, so the state outlives
// the published generations that point at it. A cell is created on first
// Acquire and linked to the derived object being built; the engine reports at
// each publish which cells the new generation links, and Expire drops cells
// left unlinked for longer than IdleTTL.
//
// Out of scope: what a cell holds, how it syncs with a store, and any timer:
// the registry runs no goroutine, the caller drives Expire.
package cell

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/engine/internal/celllink"
)

// Key identifies a cell: a type name and an id the owner builds from stable
// ids. Type keeps two cell types from colliding on one id.
type Key struct {
	Type string
	ID   string
}

// Options sets the registry's expiry policy.
type Options struct {
	// IdleTTL is how long an unlinked cell survives before Expire drops it.
	// Linked and pinned cells never expire.
	IdleTTL time.Duration
	// OnDrop is called with the cell value when it leaves the registry, so a
	// type that holds store-side state can flush it.
	OnDrop func(k Key, v any)
}

// Linker is what Acquire needs from the caller: a way to say "the object I am
// building uses this cell". The engine's transaction implements it.
type Linker interface{ Link(Key) }

// Registry owns every cell in the process. One per process.
type Registry struct {
	o     Options
	cells sync.Map // Key → *entry
	mu    sync.Mutex
	pins  map[Key]struct{}
	n     atomic.Int64
}

// unlinkedAt values: 0 linked, >0 unlinked since (unix nanos), dropped gone.
const dropped = -1

type entry struct {
	v          any
	unlinkedAt atomic.Int64
	pinned     atomic.Bool
}

// hold keeps e alive for a caller about to link it: an unlinked cell restarts
// its idle clock, so an Expire racing the caller cannot drop it.
func (e *entry) hold() bool {
	for {
		u := e.unlinkedAt.Load()
		switch {
		case u == dropped:
			return false
		case u == 0:
			return true
		case e.unlinkedAt.CompareAndSwap(u, time.Now().UnixNano()):
			return true
		}
	}
}

// NewRegistry returns an empty registry.
func NewRegistry(o Options) *Registry {
	return &Registry{o: o, pins: map[Key]struct{}{}}
}

// Acquire returns the cell for k, creating it with mk on first use, and links
// it to l's output (nil l links nothing). The returned address never changes
// while the cell is registered. mk runs under the registry lock and must not
// call Acquire.
func Acquire[T any](r *Registry, l Linker, k Key, mk func() *T) *T {
	if v, ok := r.lookup(k); ok {
		if l != nil {
			l.Link(k)
		}
		return cast[T](k, v)
	}
	r.mu.Lock()
	v, ok := r.lookup(k)
	if !ok {
		e := &entry{v: mk()}
		e.unlinkedAt.Store(time.Now().UnixNano())
		if _, pin := r.pins[k]; pin {
			e.pinned.Store(true)
		}
		r.cells.Store(k, e)
		r.n.Add(1)
		v = e.v
	}
	r.mu.Unlock()
	if l != nil {
		l.Link(k)
	}
	return cast[T](k, v)
}

func (r *Registry) lookup(k Key) (any, bool) {
	got, ok := r.cells.Load(k)
	if !ok {
		return nil, false
	}
	e := got.(*entry)
	if !e.hold() {
		return nil, false
	}
	return e.v, true
}

func cast[T any](k Key, v any) *T {
	t, ok := v.(*T)
	if !ok {
		panic(fmt.Sprintf("cell: %s/%s holds %T, acquired as %T", k.Type, k.ID, v, t))
	}
	return t
}

// Pin marks a cell as always linked, for cells no plan references (the
// revocation set). Pinning a key before its first Acquire is allowed.
func (r *Registry) Pin(k Key) {
	r.mu.Lock()
	r.pins[k] = struct{}{}
	if got, ok := r.cells.Load(k); ok {
		got.(*entry).pinned.Store(true)
	}
	r.mu.Unlock()
}

// Link pins k. It makes the registry a Linker, so Acquire(r, r, k, mk)
// obtains a cell that no plan references and that never expires.
func (r *Registry) Link(k Key) { r.Pin(k) }

// Expire drops every unpinned cell left unlinked for at least IdleTTL and
// returns how many it dropped.
func (r *Registry) Expire(now time.Time) int {
	cutoff := now.Add(-r.o.IdleTTL).UnixNano()
	var gone []Key
	var vals []any
	n := 0
	r.mu.Lock()
	r.cells.Range(func(key, val any) bool {
		e := val.(*entry)
		if e.pinned.Load() {
			return true
		}
		u := e.unlinkedAt.Load()
		if u <= 0 || u > cutoff || !e.unlinkedAt.CompareAndSwap(u, dropped) {
			return true
		}
		k := key.(Key)
		r.cells.Delete(k)
		r.n.Add(-1)
		n++
		if r.o.OnDrop != nil {
			gone = append(gone, k)
			vals = append(vals, e.v)
		}
		return true
	})
	r.mu.Unlock()
	for i, k := range gone {
		r.o.OnDrop(k, vals[i])
	}
	return n
}

// Len is the live cell count, for a gauge.
func (r *Registry) Len() int { return int(r.n.Load()) }

func init() { celllink.Publish = publishLinks }

// publishLinks applies one generation's link changes: newly linked cells stop
// their idle clock, newly unlinked ones start it.
func publishLinks(registry any, linked, unlinked any, now time.Time) {
	r, ok := registry.(*Registry)
	if !ok {
		return
	}
	for _, k := range linked.([]Key) {
		if got, ok := r.cells.Load(k); ok {
			e := got.(*entry)
			for {
				u := e.unlinkedAt.Load()
				if u <= 0 || e.unlinkedAt.CompareAndSwap(u, 0) {
					break
				}
			}
		}
	}
	ns := now.UnixNano()
	for _, k := range unlinked.([]Key) {
		if got, ok := r.cells.Load(k); ok {
			got.(*entry).unlinkedAt.CompareAndSwap(0, ns)
		}
	}
}
