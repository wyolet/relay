package modelroutes

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// File is a parsed model-routes file.
type File struct {
	// Catalog is the pinned relay-catalog release tag.
	Catalog string
	// Default is the [models] key a consumer starts on.
	Default string
	// Routes are in file order.
	Routes []Route
	// Models are in file order, which is the picker order.
	Models []ModelRoute

	path  string
	lines map[position]int
}

// Route is how a model is reached.
type Route struct {
	Name string
	// Host is a catalog host name; the model resolves as <model>@<host>. Empty leaves host choice to the catalog's default binding, for a relay that picks the real host.
	Host string
	// URL overrides the host's catalog base URL.
	URL string
	// Auth is a hint for the consumer: "key" (the default when empty), "subscription" or "none".
	Auth string
	// Adapter overrides the binding's adapter name; it may name an adapter the consumer owns.
	Adapter string
	// Passthrough lets the route carry models the catalog does not list; such a model goes upstream under its ref verbatim.
	Passthrough bool
	// Account is the identity a consumer matches a login against, such as an email. Opaque to the SDK.
	Account string
	// DisplayName is a human label for the route.
	DisplayName string
}

// ModelRoute is one [models] entry.
type ModelRoute struct {
	// Key is the picker id consumers persist, unique in the file.
	Key string
	// Model is the catalog model ref; equal to Key in the short form `key = "route"`. Empty is read as Key.
	Model string
	Route string
}

// position names a file location that problems point at: section is "routes", "models" or "" for top-level keys.
type position struct{ section, name string }

type routeTOML struct {
	Host        string `toml:"host"`
	URL         string `toml:"url"`
	Auth        string `toml:"auth"`
	Adapter     string `toml:"adapter"`
	Passthrough bool   `toml:"passthrough"`
	Account     string `toml:"account"`
	DisplayName string `toml:"displayName"`
}

type fileTOML struct {
	Catalog string                    `toml:"catalog"`
	Default string                    `toml:"default"`
	Routes  map[string]routeTOML      `toml:"routes"`
	Models  map[string]toml.Primitive `toml:"models"`
}

type inlineModelTOML struct {
	Model string `toml:"model"`
	Route string `toml:"route"`
}

// Read parses the file at path.
func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("modelroutes: %w", err)
	}
	return parse(data, path)
}

// Parse decodes a model-routes file. It checks syntax and shape only (unknown keys, value types); Resolve checks the content against a catalog.
func Parse(data []byte) (*File, error) {
	return parse(data, "")
}

func parse(data []byte, path string) (*File, error) {
	f := &File{path: path, lines: positions(scanLines(data))}
	var raw fileTOML
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, f.wrap(err)
	}
	f.Catalog, f.Default = raw.Catalog, raw.Default
	var p problems
	for _, name := range tableKeys(md, "routes") {
		r := raw.Routes[name]
		f.Routes = append(f.Routes, Route{Name: name, Host: r.Host, URL: r.URL, Auth: r.Auth, Adapter: r.Adapter, Passthrough: r.Passthrough, Account: r.Account, DisplayName: r.DisplayName})
	}
	for _, key := range tableKeys(md, "models") {
		at := position{"models", key}
		var route string
		if md.Type("models", key) == "String" && md.PrimitiveDecode(raw.Models[key], &route) == nil {
			f.Models = append(f.Models, ModelRoute{Key: key, Model: key, Route: route})
			continue
		}
		// Anything else must be a table; dotted keys give it no recorded type, so decode rather than switch on md.Type.
		var v inlineModelTOML
		if err := md.PrimitiveDecode(raw.Models[key], &v); err != nil {
			p.add(f, at, "models.%q: want a route name or { model = ..., route = ... }", key)
			continue
		}
		if v.Model == "" || v.Route == "" {
			p.add(f, at, "models.%q: needs both model and route", key)
			continue
		}
		f.Models = append(f.Models, ModelRoute{Key: key, Model: v.Model, Route: v.Route})
	}
	for _, k := range md.Undecoded() {
		at := position{name: k[0]}
		if len(k) > 1 && (k[0] == "routes" || k[0] == "models") {
			at = position{k[0], k[1]}
		}
		p.add(f, at, "unknown key %s", k.String())
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return f, nil
}

// tableKeys lists a table's direct keys in file order; the decoder's maps lose it.
func tableKeys(md toml.MetaData, table string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, k := range md.Keys() {
		if len(k) >= 2 && k[0] == table && !seen[k[1]] {
			seen[k[1]] = true
			keys = append(keys, k[1])
		}
	}
	return keys
}

func (f *File) wrap(err error) error {
	if f.path != "" {
		return fmt.Errorf("modelroutes: %s: %w", f.path, err)
	}
	return fmt.Errorf("modelroutes: %w", err)
}

// problems collects every finding so a caller fixes a file in one pass.
type problems struct{ errs []error }

func (p *problems) add(f *File, at position, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if n := f.lines[at]; n > 0 {
		msg = fmt.Sprintf("line %d: %s", n, msg)
	}
	p.errs = append(p.errs, f.wrap(errors.New(msg)))
}

func (p *problems) err() error { return errors.Join(p.errs...) }

// quote renders s as a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
