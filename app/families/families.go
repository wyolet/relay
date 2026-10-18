// Package families reads a catalog's families.yaml: the allowlist of Model
// spec.family values, each with the spec.productLine values valid under it.
// Family and line aliases only map imported values onto a name; they are
// never valid spec values. Resolving imported values is left to the importer.
package families

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type File struct {
	Families []Family `yaml:"families"`
}

type Family struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases,omitempty"`
	Lines   []Line   `yaml:"lines,omitempty"`
}

type Line struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases,omitempty"`
}

// Load reads and parses path. A read failure is returned as is (an
// *fs.PathError); a parse failure is prefixed with path.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// LineNames maps every family name to the set of its line names. Aliases
// appear in neither, so a lookup accepts only values valid in a spec.
func (f *File) LineNames() map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(f.Families))
	for _, fam := range f.Families {
		lines := make(map[string]bool, len(fam.Lines))
		for _, l := range fam.Lines {
			lines[l.Name] = true
		}
		out[fam.Name] = lines
	}
	return out
}
