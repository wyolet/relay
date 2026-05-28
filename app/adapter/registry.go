package adapter

import (
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/pipeline"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Registry holds all registered adapter specs. Built once at boot in
// cmd/relay/main.go via NewRegistry. Read-only after construction.
type Registry struct {
	specs    []*Spec
	byName   map[adapters.Name]*Spec
	adapters map[adapters.Name]pipeline.Adapter
}

// NewRegistry constructs a Registry from the given specs. Each spec must
// have had Build() called. Panics if two specs share the same Name.
func NewRegistry(specs ...*Spec) *Registry {
	r := &Registry{
		specs:    make([]*Spec, 0, len(specs)),
		byName:   make(map[adapters.Name]*Spec, len(specs)),
		adapters: make(map[adapters.Name]pipeline.Adapter, len(specs)),
	}
	for _, s := range specs {
		if _, dup := r.byName[s.Name]; dup {
			panic("adapter.Registry: duplicate spec name " + string(s.Name))
		}
		r.specs = append(r.specs, s)
		r.byName[s.Name] = s
		r.adapters[s.Name] = s.PipelineAdapter()
	}
	return r
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
