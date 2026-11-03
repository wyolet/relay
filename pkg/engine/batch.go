package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/engine/internal/celllink"
)

// batch is one drain: the input rows applied, the fixpoint, one publish.
type batch struct {
	e          *Engine
	start      time.Time
	stats      PublishStats
	changed    bool
	abandoned  bool
	linkBefore map[cell.Key]int     // link count of each touched cell before the batch
	nameRows   map[Ref]*kindInfo    // rows whose duplicate-name state may have moved
	names      map[nameKey]struct{} // names whose holders changed
}

type nameKey struct {
	k    *kindInfo
	name string
}

func (e *Engine) newBatch() *batch {
	return &batch{
		e:          e,
		start:      time.Now(),
		linkBefore: map[cell.Key]int{},
		nameRows:   map[Ref]*kindInfo{},
		names:      map[nameKey]struct{}{},
	}
}

// write sets object id of kind k to v (nil deletes) and dirties everything that depends on it. It reports whether anything changed; an identical pointer is no change.
//
// Any change of a named row dirties the readers of its old name, since a ByName reader sees the row's value and not only which row holds the name; a rename also dirties the readers of the new name.
func (b *batch) write(k *kindInfo, id string, v any) bool {
	st := &b.e.st
	old, had := st.objs[k.idx].get(id)
	if (!had && v == nil) || (had && old.v == v) {
		return false
	}
	var m Meta
	if v == nil {
		st.objs[k.idx].del(id)
	} else {
		m = k.metaOf(id, v)
		st.objs[k.idx].set(id, obj{v: v, meta: m})
	}
	b.changed = true
	b.dirtyReferrers(ByID(k.kind, id))
	if old.meta.Name != "" {
		b.dirtyReferrers(ByName(k.kind, old.meta.Name))
	}
	if m.Name != "" && m.Name != old.meta.Name {
		b.dirtyReferrers(ByName(k.kind, m.Name))
	}
	b.dirtyReferrers(Any(k.kind))
	if old.meta.Name != m.Name {
		b.rename(k, id, old.meta.Name, m.Name)
	}
	for _, w := range k.watches {
		b.rewatch(w, id, old.v, v)
	}
	return true
}

func (b *batch) rename(k *kindInfo, id, from, to string) {
	if from != "" {
		ids := b.holders(k, from)
		delete(ids, id)
		if len(ids) == 0 {
			b.e.st.names[k.idx].del(from)
		}
	}
	if to != "" {
		b.holders(k, to)[id] = struct{}{}
	}
	b.nameRows[Ref{k.kind, id}] = k
}

// holders returns the ids holding name for this batch to edit in place. The set is copied on the batch's first touch only, so published views never change and a batch adding many holders to one name stays linear.
func (b *batch) holders(k *kindInfo, name string) map[string]struct{} {
	names := &b.e.st.names[k.idx]
	nk := nameKey{k, name}
	ids, ok := names.get(name)
	if _, owned := b.names[nk]; owned && ok {
		return ids
	}
	own := make(map[string]struct{}, len(ids)+1)
	maps.Copy(own, ids)
	names.set(name, own)
	b.names[nk] = struct{}{}
	return own
}

// rewatch moves row id between memberships and dirties both its previous
// and its current outputs.
func (b *batch) rewatch(w *watch, id string, oldV, newV any) {
	var oldKeys, newKeys []string
	if oldV != nil {
		oldKeys = w.keys(oldV)
	}
	if newV != nil {
		newKeys = w.keys(newV)
	}
	for _, key := range oldKeys {
		if !slices.Contains(newKeys, key) {
			w.remove(key, id)
		}
		b.dirty(w.ctl, key)
	}
	for _, key := range newKeys {
		w.add(key, id)
		b.dirty(w.ctl, key)
	}
}

func (b *batch) dirty(c *Controller, id string) {
	if c == nil {
		return
	}
	if _, ok := c.dirty[id]; ok {
		return
	}
	c.dirty[id] = struct{}{}
	b.stats.Dirtied++
}

func (b *batch) dirtyReferrers(e Edge) {
	for r := range b.e.st.graph.referrers(e) {
		if k := b.e.sch.byKind[r.Kind]; k != nil {
			b.dirty(k.producer, r.ID)
		}
	}
}

func (b *batch) setEdges(ref Ref, st *outState, edges []Edge) {
	g := &b.e.st.graph
	diffSorted(st.edges, edges, cmpEdge,
		func(x Edge) { g.remove(x, ref); b.changed = true },
		func(x Edge) { g.add(x, ref); b.changed = true })
	st.edges = edges
}

func cmpKey(a, b cell.Key) int {
	return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.ID, b.ID))
}

func (b *batch) setLinks(st *outState, links []cell.Key) {
	diffSorted(st.links, links, cmpKey,
		func(k cell.Key) { b.addLink(k, -1) },
		func(k cell.Key) { b.addLink(k, 1) })
	st.links = links
}

func (b *batch) addLink(k cell.Key, d int) {
	counts := b.e.st.links
	if _, ok := b.linkBefore[k]; !ok {
		b.linkBefore[k] = counts[k]
	}
	if counts[k] += d; counts[k] <= 0 {
		delete(counts, k)
	}
}

func (b *batch) setIssues(o issueOwner, list []Issue) {
	if b.e.st.issues.set(o, list) {
		b.changed = true
	}
}

// markDuplicates refreshes duplicate_name on every row whose name or whose
// name's holders changed in this batch.
func (b *batch) markDuplicates() {
	for nk := range b.names {
		ids, _ := b.e.st.names[nk.k.idx].get(nk.name)
		for id := range ids {
			b.nameRows[Ref{nk.k.kind, id}] = nk.k
		}
	}
	lowest := map[nameKey][2]string{}
	for ref, k := range b.nameRows {
		var list []Issue
		if o, ok := b.e.st.objs[k.idx].get(ref.ID); ok && o.meta.Name != "" {
			if ids, _ := b.e.st.names[k.idx].get(o.meta.Name); len(ids) > 1 {
				nk := nameKey{k, o.meta.Name}
				lo, ok := lowest[nk]
				if !ok {
					lo = lowestTwo(ids)
					lowest[nk] = lo
				}
				other := lo[0]
				if other == ref.ID {
					other = lo[1]
				}
				list = []Issue{{
					Severity: SeverityError,
					Code:     "duplicate_name",
					Source:   ref,
					Target:   Ref{k.kind, other},
					Field:    "metadata.name",
					Message:  fmt.Sprintf("name %q is held by %d %s rows", o.meta.Name, len(ids), k.kind),
				}}
			}
		}
		b.setIssues(issueOwner{by: byDuplicateName, out: ref}, list)
	}
}

// lowestTwo returns the two smallest ids of a set holding at least two, so a duplicate_name issue names the same other row whatever the map order.
func lowestTwo(ids map[string]struct{}) [2]string {
	var lo [2]string
	n := 0
	for id := range ids {
		switch {
		case n == 0 || id < lo[0]:
			lo[0], lo[1] = id, lo[0]
		case n == 1 || id < lo[1]:
			lo[1] = id
		}
		n++
	}
	return lo
}

// publish freezes the working state into the next View when the batch changed anything (always when force), then reports cell links and queues the hooks.
func (b *batch) publish(force bool) {
	e := b.e
	b.markDuplicates()
	if !b.changed && !force {
		return
	}
	v := &View{
		gen:    e.view.Load().gen + 1,
		sch:    e.sch,
		kinds:  make([]objects, len(e.sch.kinds)),
		refs:   e.st.graph.freeze(),
		issues: e.st.issues.w.freeze(),
	}
	for i := range e.sch.kinds {
		v.kinds[i] = objects{e.st.objs[i].freeze(), e.st.names[i].freeze()}
	}
	e.view.Store(v)
	b.publishLinks()
	b.stats.Gen = v.gen
	b.stats.Duration = time.Since(b.start)
	if h := e.opts.Hooks.OnBatchAbandoned; h != nil && b.abandoned {
		gen, rounds := v.gen, b.stats.Rounds
		e.hooks.add(func() { h(gen, rounds) })
	}
	if h := e.opts.Hooks.OnPublish; h != nil {
		s := b.stats
		e.hooks.add(func() { h(v, s) })
	}
}

func (b *batch) publishLinks() {
	if b.e.opts.Cells == nil || celllink.Publish == nil || len(b.linkBefore) == 0 {
		return
	}
	var linked, unlinked []cell.Key
	for k, before := range b.linkBefore {
		after := b.e.st.links[k]
		switch {
		case before == 0 && after > 0:
			linked = append(linked, k)
		case before > 0 && after == 0:
			unlinked = append(unlinked, k)
		}
	}
	celllink.Publish(b.e.opts.Cells, linked, unlinked, time.Now())
}
