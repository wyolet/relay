// Package settings is the typed-sectioned config layer.
//
// Each known section is a Go type with a Validate() method, registered
// statically in this package's init via Register(). The DB stores one
// row per section in the `settings` table; the value is opaque JSONB,
// the typed struct enforces shape on read and write.
//
// Adding a new section: create <section>.go with the typed struct,
// implement Validate() error, call Register() from its init().
//
// Hot-path consumers read from the in-memory Cache populated by the
// catalog reconciler; the Store is admin-plane only.
package settings

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Section describes one known settings section. Defaults returns a
// fresh zero-value instance; Decode parses raw JSON into a typed value
// and validates it; SchemaRef is the OpenAPI component name of the
// typed value's schema (set automatically by the per-section
// registerSettingsSection helper); Description is operator-facing
// prose explaining the section's purpose.
type Section struct {
	Name        string
	Description string
	Defaults    func() any
	Decode      func([]byte) (any, error)
	SchemaRef   string
}

var (
	mu       sync.RWMutex
	registry = map[string]Section{}
)

// Register adds sec to the section registry. Called from each section's
// init(). Panics on duplicate registration — the typo would otherwise be
// silent.
func Register(sec Section) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[sec.Name]; dup {
		panic(fmt.Sprintf("settings: section %q registered twice", sec.Name))
	}
	registry[sec.Name] = sec
}

// Lookup returns the registered section by name. ok=false means the
// caller asked for an unknown section — handler returns 404.
func Lookup(name string) (Section, bool) {
	mu.RLock()
	defer mu.RUnlock()
	sec, ok := registry[name]
	return sec, ok
}

// Names returns all registered section names in lexical order. Used by
// the list endpoint and the cache to enumerate sections.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SetSchemaRef records the OpenAPI component name for section's typed
// value. Called by the HTTP layer right after huma registers the typed
// GET/PUT pair, so the per-section schema id is discoverable via the
// list / sections endpoints.
func SetSchemaRef(name, ref string) {
	mu.Lock()
	defer mu.Unlock()
	sec, ok := registry[name]
	if !ok {
		return
	}
	sec.SchemaRef = ref
	registry[name] = sec
}

// validator is the contract every section value must satisfy. Sections
// register concrete pointer types whose Validate() is invoked before
// any DB write.
type validator interface {
	Validate() error
}

// decodeAndValidate parses raw into a fresh instance of T and validates
// it. Helper used by per-section Decode funcs.
func decodeAndValidate[T any, PT interface {
	*T
	validator
}](raw []byte) (any, error) {
	var v T
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
	}
	if err := PT(&v).Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}
