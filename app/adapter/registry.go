package adapter

import (
	"fmt"
	"reflect"

	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/adapters"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Registry holds all registered adapter specs. Built once at boot in
// cmd/relay via NewRegistry over internal/adapterspecs. Read-only after construction.
type Registry struct {
	specs           []*Spec
	byName          map[adapters.Name]*Spec
	adapters        map[adapters.Name]pipeline.Adapter
	translatorGroup map[adapters.Name]int
}

// NewRegistry constructs a Registry from the given specs. Each spec must
// have had Build() called. Panics if two specs share the same Name.
func NewRegistry(specs ...*Spec) *Registry {
	r := &Registry{
		specs:           make([]*Spec, 0, len(specs)),
		byName:          make(map[adapters.Name]*Spec, len(specs)),
		adapters:        make(map[adapters.Name]pipeline.Adapter, len(specs)),
		translatorGroup: make(map[adapters.Name]int, len(specs)),
	}
	for _, s := range specs {
		if _, dup := r.byName[s.Name]; dup {
			panic("adapter.Registry: duplicate spec name " + string(s.Name))
		}
		r.translatorGroup[s.Name] = r.groupOf(s.Translator)
		r.specs = append(r.specs, s)
		r.byName[s.Name] = s
		r.adapters[s.Name] = s.PipelineAdapter()
	}
	return r
}

// groupOf returns the translator group of t among the specs registered so far: the group of the first one whose translator equals t, else a new one; 0 for a nil t.
func (r *Registry) groupOf(t v1.Translator) int {
	if t == nil {
		return 0
	}
	for _, s := range r.specs {
		if s.Translator != nil && sameTranslator(t, s.Translator) {
			return r.translatorGroup[s.Name]
		}
	}
	return len(r.specs) + 1
}

// sameTranslator reports whether a and b are one translator value. Unlike ==, it cannot panic: a value holding an uncomparable one (a slice behind an interface field) equals nothing.
func sameTranslator(a, b v1.Translator) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Type() == vb.Type() && va.Comparable() && vb.Comparable() && va.Equal(vb)
}

// TranslatorGroup returns the group of name's translator: two specs share a group when they share a translator value, so one body that translator serialized serves both. 0 when name has no translator or is unregistered.
func (r *Registry) TranslatorGroup(name adapters.Name) int {
	if r == nil {
		return 0
	}
	return r.translatorGroup[name]
}

// Spec returns the Spec for the given name, or nil if unregistered.
func (r *Registry) Spec(name adapters.Name) *Spec {
	if r == nil {
		return nil
	}
	return r.byName[name]
}

// PipelineAdapter returns the pipeline.Adapter for the given name, or nil.
func (r *Registry) PipelineAdapter(name adapters.Name) pipeline.Adapter {
	if r == nil {
		return nil
	}
	return r.adapters[name]
}

// AdapterMap returns a map[adapters.Name]pipeline.Adapter for all registered
// specs. The returned map is a copy; mutation does not affect the registry.
func (r *Registry) AdapterMap() map[adapters.Name]pipeline.Adapter {
	out := make(map[adapters.Name]pipeline.Adapter, len(r.adapters))
	for k, v := range r.adapters {
		out[k] = v
	}
	return out
}

// TranslatorMap returns a v1.Registry (map[v1.Name]v1.Translator) for all
// specs that have a non-nil Translator. Specs with BytePass=true and nil
// Translator are omitted — they have no canonical translator.
func (r *Registry) TranslatorMap() v1.Registry {
	out := make(v1.Registry)
	for _, s := range r.specs {
		if s.Translator != nil {
			out[v1.Name(s.Name)] = s.Translator
		}
	}
	return out
}

// Specs returns all registered specs in registration order.
func (r *Registry) Specs() []*Spec {
	return r.specs
}

// AssertWired verifies the composition root registered a coherent set of
// specs, so the defensive "no spec / no adapter / no translator" 500s in
// the dispatch path are structurally unreachable. The composition root
// calls this at boot and fails fast on a violation — a wiring bug must
// crash the binary, never surface as a runtime request error.
//
// It enforces these invariants:
//   - Every upstream-binding adapter (the allow-set HostBinding.Adapter is
//     validated against) has a registered spec with a non-nil pipeline
//     adapter to dispatch to.
//   - Every inbound, non-byte-pass spec has a canonical Translator, so a
//     cross-shape route can always translate.
//   - Every Family names a registered spec that has no Family of its own
//     (FamilyName does not follow chains).
func (r *Registry) AssertWired() error {
	for _, n := range adapters.UpstreamBindingNames() {
		s := r.byName[n]
		if s == nil {
			return fmt.Errorf("adapter.Registry: upstream binding %q has no registered spec", n)
		}
		if r.adapters[n] == nil {
			return fmt.Errorf("adapter.Registry: upstream binding %q has no pipeline adapter", n)
		}
	}
	for _, s := range r.specs {
		if len(s.InboundPaths) > 0 && !s.BytePass && s.Translator == nil {
			return fmt.Errorf("adapter.Registry: inbound spec %q has no canonical translator", s.Name)
		}
		if s.Family != "" {
			f := r.byName[s.Family]
			if f == nil {
				return fmt.Errorf("adapter.Registry: spec %q names unregistered family %q", s.Name, s.Family)
			}
			if f.Family != "" {
				return fmt.Errorf("adapter.Registry: spec %q names family %q, which is itself in family %q", s.Name, s.Family, f.Family)
			}
		}
	}
	return nil
}
