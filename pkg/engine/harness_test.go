package engine_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

// parent and item are the two input kinds; outA, outB and outC are derived kinds.
type parent struct {
	ID, Name string
	Ver      int64
	Val      int
}

type item struct {
	ID         string
	Ver        int64
	Val        int
	Parent     string // read by id
	ParentName string // read by name
	Group      string // Watch key
	Note       string // issue code to raise, when set
}

type outA struct {
	V    int
	S    string
	Cell *atomic.Int64
}

type (
	outB outA
	outC outA
)

const (
	kParent engine.Kind = "parent"
	kItem   engine.Kind = "item"
	kA      engine.Kind = "a"
	kB      engine.Kind = "b"
	kC      engine.Kind = "c"
)

func refOf(k engine.Kind, id string) engine.Ref { return engine.Ref{Kind: k, ID: id} }

// memSrc is an in-memory Source that counts Get calls per id.
type memSrc[T any] struct {
	mu   sync.Mutex
	id   func(*T) string
	rows map[string]*T
	gets map[string]int
}

func newMemSrc[T any](id func(*T) string) *memSrc[T] {
	return &memSrc[T]{id: id, rows: map[string]*T{}, gets: map[string]int{}}
}

func (s *memSrc[T]) List(context.Context) ([]*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.rows))
	for id := range s.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*T, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.rows[id])
	}
	return out, nil
}

func (s *memSrc[T]) Get(_ context.Context, id string) (*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets[id]++
	return s.rows[id], nil
}

func (s *memSrc[T]) set(rows ...*T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		s.rows[s.id(r)] = r
	}
}

func (s *memSrc[T]) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
}

func (s *memSrc[T]) getCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[id]
}

// tally counts reconcile calls per controller and output id.
type tally struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *tally) hit(ctl, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[ctl+"/"+id]++
}

func (c *tally) count(ctl, id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[ctl+"/"+id]
}

func (c *tally) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n = nil
}

// rig is an engine with the parent, item, a, b and c kinds registered. Controllers are added between newRig and load.
type rig struct {
	t       *testing.T
	e       *engine.Engine
	parents *memSrc[parent]
	items   *memSrc[item]
	calls   *tally
	pubs    chan engine.PublishStats
	closed  bool
}

func newRig(t *testing.T, o engine.Options) *rig {
	t.Helper()
	r := &rig{
		t:       t,
		parents: newMemSrc(func(p *parent) string { return p.ID }),
		items:   newMemSrc(func(i *item) string { return i.ID }),
		calls:   &tally{},
		pubs:    make(chan engine.PublishStats, 1024),
	}
	inner := o.Hooks.OnPublish
	o.Hooks.OnPublish = func(v *engine.View, s engine.PublishStats) {
		if inner != nil {
			inner(v, s)
		}
		r.pubs <- s
	}
	r.e = engine.New(o)
	engine.RegisterKind(r.e, engine.KindSpec[parent]{
		Kind:   kParent,
		Meta:   func(p *parent) engine.Meta { return engine.Meta{ID: p.ID, Name: p.Name, Version: p.Ver} },
		Source: r.parents,
	})
	engine.RegisterKind(r.e, engine.KindSpec[item]{
		Kind:   kItem,
		Meta:   func(i *item) engine.Meta { return engine.Meta{ID: i.ID, Version: i.Ver} },
		Source: r.items,
	})
	engine.RegisterKind(r.e, engine.KindSpec[outA]{Kind: kA})
	engine.RegisterKind(r.e, engine.KindSpec[outB]{Kind: kB})
	engine.RegisterKind(r.e, engine.KindSpec[outC]{Kind: kC})
	return r
}

// load boots the engine, consumes the generation-1 publish and resets the reconcile tally.
func (r *rig) load() {
	r.t.Helper()
	if err := r.e.Load(context.Background()); err != nil {
		r.t.Fatalf("Load: %v", err)
	}
	r.t.Cleanup(func() {
		if !r.closed {
			_ = r.e.Close()
		}
	})
	if s := r.nextPub(); s.Gen != 1 {
		r.t.Fatalf("first publish gen = %d, want 1", s.Gen)
	}
	r.calls.reset()
}

func (r *rig) close() {
	r.closed = true
	if err := r.e.Close(); err != nil {
		r.t.Fatalf("Close: %v", err)
	}
}

func (r *rig) view() *engine.View { return r.e.View() }

func (r *rig) putParent(p *parent) {
	r.t.Helper()
	r.parents.set(p)
	if err := engine.Apply(r.e, p); err != nil {
		r.t.Fatalf("Apply parent %s: %v", p.ID, err)
	}
}

func (r *rig) putItem(i *item) {
	r.t.Helper()
	r.items.set(i)
	if err := engine.Apply(r.e, i); err != nil {
		r.t.Fatalf("Apply item %s: %v", i.ID, err)
	}
}

func (r *rig) delParent(id string) {
	r.t.Helper()
	r.parents.remove(id)
	if err := engine.Delete(r.e, refOf(kParent, id)); err != nil {
		r.t.Fatalf("Delete parent %s: %v", id, err)
	}
}

func (r *rig) delItem(id string) {
	r.t.Helper()
	r.items.remove(id)
	if err := engine.Delete(r.e, refOf(kItem, id)); err != nil {
		r.t.Fatalf("Delete item %s: %v", id, err)
	}
}

func (r *rig) nextPub() engine.PublishStats {
	r.t.Helper()
	select {
	case s := <-r.pubs:
		return s
	case <-time.After(5 * time.Second):
		r.t.Fatal("no publish within 5s")
		return engine.PublishStats{}
	}
}

func (r *rig) expectNoPub(d time.Duration) {
	r.t.Helper()
	select {
	case s := <-r.pubs:
		r.t.Fatalf("unexpected publish of gen %d", s.Gen)
	case <-time.After(d):
	}
}

// eventually polls cond until it holds or 3s pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func selfKey(i *item) []string { return []string{i.ID} }

// ctlByID builds one outA per item from the parent it names by id; a missing parent drops the output with ref_missing attributed to the item.
func ctlByID(r *rig) *engine.Controller {
	c := engine.NewController("by-id", kA, func(tx *engine.Txn, id string) error {
		r.calls.hit("by-id", id)
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		p := engine.Get[parent](tx, it.Parent)
		if p == nil {
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: "ref_missing", Source: refOf(kItem, id), Target: refOf(kParent, it.Parent), Field: "parent"})
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &outA{V: p.Val, S: p.Name})
		return nil
	})
	engine.Watch(c, selfKey)
	return c
}

// ctlByName builds one outB per item from the parent it names by slug; outB.V is -1 when the name resolves to nothing.
func ctlByName(r *rig) *engine.Controller {
	c := engine.NewController("by-name", kB, func(tx *engine.Txn, id string) error {
		r.calls.hit("by-name", id)
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		out := &outB{V: -1}
		if p := engine.ByNameOf[parent](tx, it.ParentName); p != nil {
			out = &outB{V: p.Val, S: p.ID}
		}
		engine.Put(tx, out)
		return nil
	})
	engine.Watch(c, selfKey)
	return c
}

func viewA(v *engine.View, id string) *outA { return engine.Get[outA](v, id) }
func viewB(v *engine.View, id string) *outB { return engine.Get[outB](v, id) }
func viewC(v *engine.View, id string) *outC { return engine.Get[outC](v, id) }

func issueCodes(is []engine.Issue) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Code)
	}
	return out
}
