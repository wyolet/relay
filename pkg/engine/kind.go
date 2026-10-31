package engine

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
)

// Kind names a class of objects. A plain string keeps Ref and Edge
// comparable values instead of type parameters.
type Kind string

// Ref identifies one object: its kind and id. Every graph edge, issue and
// dirty entry needs one comparable identity.
type Ref struct {
	Kind Kind
	ID   string
}

func cmpRef(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
}

// Edge is a dependency target in one of three forms: by id, by name, or any
// row of a kind (for patterns and wildcards that cannot name a row).
type Edge struct {
	Kind Kind
	ID   string // ByID
	Name string // ByName
	// all marks Any, so ByID(k, "") of a singleton object stays distinct.
	all bool
}

// ByID is an edge to one object by id.
func ByID(k Kind, id string) Edge { return Edge{Kind: k, ID: id} }

// ByName is an edge to whichever object of kind k holds name. Any change to that object dirties the reader, not only a rename.
func ByName(k Kind, name string) Edge { return Edge{Kind: k, Name: name} }

// Any is an edge to every object of kind k.
func Any(k Kind) Edge { return Edge{Kind: k, all: true} }

func cmpEdge(a, b Edge) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Name, b.Name), cmpBool(a.all, b.all))
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// Meta is what the engine needs to know about any object, so it never reads
// a domain type's own metadata.
type Meta struct {
	ID string
	// Name is the slug; empty for kinds without one. A name held by two rows
	// of one kind resolves to neither and raises duplicate_name on both.
	Name string
	// Version is the row's resource version. 0 means unknown: always fetch,
	// never deduplicate.
	Version int64
}

// Source lists and fetches one input kind from its backing store, so the
// engine owns the load and resync loops without importing storage.
type Source[T any] interface {
	List(ctx context.Context) ([]*T, error)
	// Get returns (nil, nil) for a missing row, which the engine applies as a
	// delete.
	Get(ctx context.Context, id string) (*T, error)
}

// KindSpec registers one kind as a value rather than a method chain.
type KindSpec[T any] struct {
	Kind Kind
	// Meta is required for input kinds. For derived kinds it is optional and
	// only supplies Name and Version; the id is always the output id.
	Meta   func(*T) Meta
	Source Source[T] // nil for derived kinds (only a controller writes them)
	// Shards is the number of copy-on-write partitions of this kind's map, so
	// a write to a large kind copies one partition; 1 when unset.
	Shards int
}

// RegisterKind declares a kind before Load. Panics after Load: kinds are fixed
// for the engine's life. One Go type maps to exactly one kind.
func RegisterKind[T any](e *Engine, spec KindSpec[T]) {
	e.mustBeUnloaded("RegisterKind")
	typ := reflect.TypeFor[T]()
	switch {
	case spec.Kind == "":
		panic("engine: RegisterKind with an empty Kind")
	case e.sch.byKind[spec.Kind] != nil:
		panic(fmt.Sprintf("engine: kind %q registered twice", spec.Kind))
	case e.sch.byType[typ] != nil:
		panic(fmt.Sprintf("engine: type %v registered as two kinds", typ))
	case spec.Source != nil && spec.Meta == nil:
		panic(fmt.Sprintf("engine: input kind %q needs Meta", spec.Kind))
	}
	k := &kindInfo{idx: len(e.sch.kinds), kind: spec.Kind, typ: typ, shards: max(spec.Shards, 1)}
	if m := spec.Meta; m != nil {
		k.meta = func(v any) Meta { return m(v.(*T)) }
	}
	if src := spec.Source; src != nil {
		k.list = func(ctx context.Context) ([]any, error) {
			rows, err := src.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(rows))
			for _, r := range rows {
				if r != nil {
					out = append(out, r)
				}
			}
			return out, nil
		}
		k.get = func(ctx context.Context, id string) (any, error) {
			row, err := src.Get(ctx, id)
			if err != nil || row == nil {
				return nil, err
			}
			return row, nil
		}
	}
	e.sch.kinds = append(e.sch.kinds, k)
	e.sch.byKind[k.kind] = k
	e.sch.byType[typ] = k
}

// Generation numbers each published View; strictly increasing, 0 is the
// pre-boot empty view.
type Generation uint64

// Op is what happened to an input row.
type Op uint8

const (
	OpUpsert Op = iota + 1
	OpDelete
)

// Event is one change notification, decoded by the caller. Version 0 forces
// a fetch; a non-zero Version at or below the held one is dropped unfetched.
type Event struct {
	Ref     Ref
	Op      Op
	Version int64
}

type kindInfo struct {
	idx      int
	kind     Kind
	typ      reflect.Type
	shards   int
	meta     func(any) Meta
	list     func(context.Context) ([]any, error) // nil for derived kinds
	get      func(context.Context, string) (any, error)
	watches  []*watch
	producer *Controller
}

func (k *kindInfo) input() bool { return k.list != nil }

// metaOf returns v's Meta; a derived object's id is always its output id.
func (k *kindInfo) metaOf(id string, v any) Meta {
	if k.meta == nil {
		return Meta{ID: id}
	}
	m := k.meta(v)
	m.ID = id
	return m
}

type schema struct {
	kinds    []*kindInfo
	byKind   map[Kind]*kindInfo
	byType   map[reflect.Type]*kindInfo
	ctls     []*Controller
	resolved bool
}

func newSchema() *schema {
	return &schema{byKind: map[Kind]*kindInfo{}, byType: map[reflect.Type]*kindInfo{}}
}

func kindOf[T any](s *schema) *kindInfo {
	k := s.byType[reflect.TypeFor[T]()]
	if k == nil {
		panic(fmt.Sprintf("engine: no kind registered for %v", reflect.TypeFor[T]()))
	}
	return k
}
