package engine

import (
	"fmt"
	"reflect"
	"slices"
)

// Controller is one derived kind plus the function that builds one of its
// objects from current state.
type Controller struct {
	name      string
	produces  Kind
	reconcile func(*Txn, string) error
	watches   []*watch
	kind      *kindInfo // resolved at Load
	dirty     map[string]struct{}
}

// watch is one Watch membership: rows of kind are grouped under the output ids
// keys maps them to.
type watch struct {
	ctl   *Controller
	typ   reflect.Type
	keys  func(any) []string
	kind  *kindInfo
	byOut map[string]map[string]struct{} // output id → member row ids
}

// NewController declares a controller producing kind produces. reconcile
// builds output id from current state and either Puts it, Drops it, or returns
// an error. Controllers run in registration order within a round; register
// parents before children.
func NewController(name string, produces Kind, reconcile func(tx *Txn, id string) error) *Controller {
	if name == "" || produces == "" || reconcile == nil {
		panic("engine: NewController needs a name, a kind and a reconcile func")
	}
	return &Controller{name: name, produces: produces, reconcile: reconcile, dirty: map[string]struct{}{}}
}

// Watch maps a row of kind T to the output ids it belongs to. The engine keeps
// the reverse index and dirties an output when its membership changes; an
// output with no members that drops is forgotten.
func Watch[T any](c *Controller, keys func(row *T) []string) {
	if c.kind != nil {
		panic("engine: Watch after Load")
	}
	typ := reflect.TypeFor[T]()
	for _, w := range c.watches {
		if w.typ == typ {
			panic(fmt.Sprintf("engine: controller %q watches %v twice", c.name, typ))
		}
	}
	c.watches = append(c.watches, &watch{
		ctl:   c,
		typ:   typ,
		keys:  func(v any) []string { return keys(v.(*T)) },
		byOut: map[string]map[string]struct{}{},
	})
}

// Register adds the controller; panics after Load.
func (e *Engine) Register(c *Controller) {
	e.mustBeUnloaded("Register")
	for _, o := range e.sch.ctls {
		if o == c || o.name == c.name {
			panic(fmt.Sprintf("engine: controller %q registered twice", c.name))
		}
	}
	e.sch.ctls = append(e.sch.ctls, c)
}

// resolve binds controllers and watches to registered kinds once, at Load.
func (s *schema) resolve() {
	if s.resolved {
		return
	}
	s.resolved = true
	for _, c := range s.ctls {
		k := s.byKind[c.produces]
		switch {
		case k == nil:
			panic(fmt.Sprintf("engine: controller %q produces unregistered kind %q", c.name, c.produces))
		case k.input():
			panic(fmt.Sprintf("engine: controller %q produces input kind %q", c.name, c.produces))
		case k.producer != nil:
			panic(fmt.Sprintf("engine: kind %q has two controllers", c.produces))
		}
		k.producer = c
		c.kind = k
		for _, w := range c.watches {
			wk := s.byType[w.typ]
			if wk == nil {
				panic(fmt.Sprintf("engine: controller %q watches unregistered type %v", c.name, w.typ))
			}
			w.kind = wk
			wk.watches = append(wk.watches, w)
		}
	}
}

func (c *Controller) watchOf(typ reflect.Type) *watch {
	for _, w := range c.watches {
		if w.typ == typ {
			return w
		}
	}
	panic(fmt.Sprintf("engine: controller %q has no Watch for %v", c.name, typ))
}

func (c *Controller) hasMembers(id string) bool {
	for _, w := range c.watches {
		if len(w.byOut[id]) > 0 {
			return true
		}
	}
	return false
}

func (w *watch) members(out string) []string {
	ids := make([]string, 0, len(w.byOut[out]))
	for id := range w.byOut[out] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (w *watch) add(out, id string) {
	m := w.byOut[out]
	if m == nil {
		m = map[string]struct{}{}
		w.byOut[out] = m
	}
	m[id] = struct{}{}
}

func (w *watch) remove(out, id string) {
	m := w.byOut[out]
	delete(m, id)
	if len(m) == 0 {
		delete(w.byOut, out)
	}
}
