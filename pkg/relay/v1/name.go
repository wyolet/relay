package v1

// Name identifies an adapter at runtime. Catalog data references adapters
// by name; the registry maps names to live Translator instances.
type Name string

// Registry maps an adapter Name to its Translator. The composition root
// populates the registry; consumers look up by name. Read-only after startup.
type Registry map[Name]Translator

// Get returns the translator for name, or nil if absent.
func (r Registry) Get(name Name) Translator {
	return r[name]
}
