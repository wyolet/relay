package engine

import (
	"fmt"
	"iter"
	"reflect"
	"slices"

	"github.com/wyolet/relay/pkg/engine/cell"
)

// Txn is the write-side transaction one reconcile runs in. Reads record
// edges; writes stage into the batch; nothing is visible until publish.
type Txn struct {
	e      *Engine
	ctl    *Controller
	out    Ref
	edges  []Edge
	links  []cell.Key
	issues []Issue
	staged any // the Put object, nil for Drop
	done   bool
}

// Out is the Ref being reconciled, for issue attribution and cell keys.
func (tx *Txn) Out() Ref { return tx.out }

// Issue attaches a diagnostic to this reconcile; the set replaces the previous
// reconcile's set for this output. Source defaults to Out().
func (tx *Txn) Issue(i Issue) {
	if i.Source == (Ref{}) {
		i.Source = tx.out
	}
	tx.issues = append(tx.issues, i)
}

// Link records that the output uses a cell, so the registry keeps it alive
// while this output is published. A no-op when the engine has no registry.
func (tx *Txn) Link(k cell.Key) {
	if tx.e.opts.Cells != nil {
		tx.links = append(tx.links, k)
	}
}

// record adds a dependency edge; reading one's own previous output is not a
// dependency, or every changed output would reconcile twice.
func (tx *Txn) record(e Edge) {
	if !e.all && e.Name == "" && e.Kind == tx.out.Kind && e.ID == tx.out.ID {
		return
	}
	tx.edges = append(tx.edges, e)
}

func (tx *Txn) stage(v any) {
	if tx.done {
		panic(fmt.Sprintf("engine: %s: second Put or Drop for %s/%s", tx.ctl.name, tx.out.Kind, tx.out.ID))
	}
	tx.done = true
	tx.staged = v
}

// Get reads one object of kind T by id: from a Txn's working state (recording
// ByID) or from a View. R is inferred from the first argument. nil when absent.
func Get[T any, R *Txn | *View](r R, id string) *T {
	switch r := any(r).(type) {
	case *Txn:
		k := kindOf[T](r.e.sch)
		r.record(ByID(k.kind, id))
		return as[T](r.e.objects(k).get(id))
	case *View:
		return as[T](r.objects(kindOf[T](r.sch)).get(id))
	}
	return nil
}

// ByNameOf reads by slug, recording ByName on a Txn. nil when absent or when
// two rows share the name.
func ByNameOf[T any, R *Txn | *View](r R, name string) *T {
	switch r := any(r).(type) {
	case *Txn:
		k := kindOf[T](r.e.sch)
		r.record(ByName(k.kind, name))
		return as[T](r.e.objects(k).byName(name))
	case *View:
		return as[T](r.objects(kindOf[T](r.sch)).byName(name))
	}
	return nil
}

// All iterates every object of kind T in id order, recording Any on a Txn.
func All[T any, R *Txn | *View](r R) iter.Seq[*T] {
	var o objects
	switch r := any(r).(type) {
	case *Txn:
		k := kindOf[T](r.e.sch)
		r.record(Any(k.kind))
		o = r.e.objects(k)
	case *View:
		o = r.objects(kindOf[T](r.sch))
	}
	ids := o.sortedIDs()
	return func(yield func(*T) bool) {
		for _, id := range ids {
			if !yield(as[T](o.get(id))) {
				return
			}
		}
	}
}

// Members returns the rows of kind T whose Watch key function maps to the
// output being reconciled, sorted by id. It records nothing: membership
// changes dirty the output by themselves.
func Members[T any](tx *Txn) []*T {
	w := tx.ctl.watchOf(reflect.TypeFor[T]())
	o := tx.e.objects(w.kind)
	ids := w.members(tx.out.ID)
	out := make([]*T, 0, len(ids))
	for _, id := range ids {
		if v := as[T](o.get(id)); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// Put stages the output of this reconcile; a nil out is a Drop. Exactly one
// Put or Drop per reconcile; a second call panics.
func Put[T any](tx *Txn, out *T) {
	if k := kindOf[T](tx.e.sch); k != tx.ctl.kind {
		panic(fmt.Sprintf("engine: %s produces %s, Put got %s", tx.ctl.name, tx.ctl.produces, k.kind))
	}
	if out == nil {
		tx.stage(nil)
		return
	}
	tx.stage(out)
}

// Drop stages the absence of the output.
func Drop(tx *Txn) { tx.stage(nil) }

func as[T any](v any) *T {
	if v == nil {
		return nil
	}
	return v.(*T)
}

// objects is one kind's maps in some state: a view's or the working batch's.
type objects struct {
	objs  *shardMap[string, obj]
	names *shardMap[string, []string]
}

type obj struct {
	v    any
	meta Meta
}

func (o objects) get(id string) any {
	if o.objs == nil {
		return nil
	}
	x, _ := o.objs.get(id)
	return x.v
}

func (o objects) byName(name string) any {
	if o.names == nil {
		return nil
	}
	ids, _ := o.names.get(name)
	if len(ids) != 1 {
		return nil
	}
	return o.get(ids[0])
}

func (o objects) sortedIDs() []string {
	if o.objs == nil {
		return nil
	}
	ids := o.objs.keys()
	slices.Sort(ids)
	return ids
}
