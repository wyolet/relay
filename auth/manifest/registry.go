package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// Resolver resolves a name to an id. Kind is a document kind ("Team", "User") or one of the product's own kinds a product field refers to.
type Resolver interface {
	ID(kind, name string) (string, bool)
}

// Namer resolves an id back to a name, the reverse of Resolver.
type Namer interface {
	Name(kind, id string) (string, bool)
}

// Field is a product field under one kind's spec. Its value is stored in the row's Spec.Attributes under Attribute, so the directory round-trips it untouched.
type Field struct {
	// Name is the key under spec in a document.
	Name string
	// Attribute is the key in Spec.Attributes; empty means Name.
	Attribute string
	// Schema is the JSON Schema of the value, merged into the kind's schema.
	Schema map[string]any
	// ToAttribute checks the document's value and returns what the row stores; refs resolves names the value holds. A nil result leaves the attribute out. Nil stores the value as written.
	ToAttribute func(value json.RawMessage, refs Resolver) (json.RawMessage, error)
	// FromAttribute renders the stored attribute as the document's value; names resolves the ids it holds. Nil renders it as stored.
	FromAttribute func(stored json.RawMessage, names Namer) (json.RawMessage, error)
}

func (f Field) attribute() string {
	if f.Attribute != "" {
		return f.Attribute
	}
	return f.Name
}

// Config is what a product registers.
type Config struct {
	// RuleKinds and RuleVerbs are the terms a role rule may name besides the wildcard. Nil accepts any term, which leaves a typo silently matching nothing.
	RuleKinds []string
	RuleVerbs []string
	// Fields are the product's spec fields, by document kind.
	Fields map[string][]Field
	// APIVersions are further apiVersions accepted for these kinds, for products that published them under their own group first.
	APIVersions []string
	// LookupEnv reads a password's environment variable; nil is os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Registry parses, translates and renders documents under one product's Config. It holds no state beyond the Config and is safe for concurrent use.
type Registry struct {
	cfg Config
}

// NewRegistry checks cfg: a field must name a kind of this package and must not shadow a built-in spec field or another product field.
func NewRegistry(cfg Config) (*Registry, error) {
	for kind, fields := range cfg.Fields {
		if !IsKind(kind) {
			return nil, fmt.Errorf("manifest: fields registered for unknown kind %q", kind)
		}
		builtin := specKeys(kind)
		seen := map[string]bool{}
		for _, f := range fields {
			switch {
			case f.Name == "":
				return nil, fmt.Errorf("manifest: %s: a field needs a name", kind)
			case slices.Contains(builtin, f.Name):
				return nil, fmt.Errorf("manifest: %s: field %q is built in", kind, f.Name)
			case seen[f.Name] || seen[f.attribute()]:
				return nil, fmt.Errorf("manifest: %s: field %q is registered twice", kind, f.Name)
			}
			seen[f.Name], seen[f.attribute()] = true, true
		}
	}
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	return &Registry{cfg: cfg}, nil
}

// MustRegistry is NewRegistry for a Config fixed at compile time; it panics on an invalid one.
func MustRegistry(cfg Config) *Registry {
	r, err := NewRegistry(cfg)
	if err != nil {
		panic(err)
	}
	return r
}

// AcceptsAPIVersion reports whether v is APIVersion or one the product added.
func (r *Registry) AcceptsAPIVersion(v string) bool {
	return v == APIVersion || slices.Contains(r.cfg.APIVersions, v)
}

func (r *Registry) fields(kind string) []Field { return r.cfg.Fields[kind] }

// toAttributes turns the document's product fields into stored attributes.
func (r *Registry) toAttributes(d *Document, refs Resolver) (map[string]json.RawMessage, error) {
	var attrs map[string]json.RawMessage
	for _, f := range r.fields(d.Kind) {
		value, ok := d.Fields[f.Name]
		if !ok {
			continue
		}
		stored := value
		if f.ToAttribute != nil {
			var err error
			if stored, err = f.ToAttribute(value, refs); err != nil {
				return nil, docError(d, "spec."+f.Name, err)
			}
		}
		if stored == nil {
			continue
		}
		if attrs == nil {
			attrs = map[string]json.RawMessage{}
		}
		attrs[f.attribute()] = stored
	}
	return attrs, nil
}

// fromAttributes renders stored attributes as the document's product fields. An attribute no field claims is not rendered: a document has nowhere to put it.
func (r *Registry) fromAttributes(kind string, attrs map[string]json.RawMessage, names Namer) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	for _, f := range r.fields(kind) {
		stored, ok := attrs[f.attribute()]
		if !ok {
			continue
		}
		value := stored
		if f.FromAttribute != nil {
			var err error
			if value, err = f.FromAttribute(stored, names); err != nil {
				return nil, fmt.Errorf("spec.%s: %w", f.Name, err)
			}
		}
		if value == nil {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[f.Name] = value
	}
	return out, nil
}

// lookup resolves through refs, treating a nil Resolver as knowing nothing.
func lookup(refs Resolver, kind, name string) (string, bool) {
	if refs == nil {
		return "", false
	}
	return refs.ID(kind, name)
}

// nameOf resolves through names, falling back to the id so a dangling reference still renders as something an operator can trace.
func nameOf(names Namer, kind, id string) string {
	if names != nil {
		if n, ok := names.Name(kind, id); ok {
			return n
		}
	}
	return id
}
