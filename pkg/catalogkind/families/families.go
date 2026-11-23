// Package families reads a catalog's families.yaml: the allowlist of Model
// spec.family values, each with the spec.productLine values valid under it
// and the logo its models share. Family and line aliases only map imported
// values onto a name; they are never valid spec values. Resolving imported
// values is left to the importer.
package families

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/pkg/catalogkind/manifest"
	"github.com/wyolet/relay/pkg/meta"
)

// FileName is the families file of a catalog data tree, at its root.
const FileName = "families.yaml"

type File struct {
	Families []Family `yaml:"families"`
}

type Family struct {
	Name    string     `yaml:"name"`
	Aliases []string   `yaml:"aliases,omitempty"`
	Lines   []Line     `yaml:"lines,omitempty"`
	Icon    *meta.Icon `yaml:"icon,omitempty"`
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

// LoadTree reads the families file of the data tree at dir. A tree without
// one has no families.
func LoadTree(dir string) (*File, error) {
	f, err := Load(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &File{}, nil
	}
	return f, err
}

// FillModelIcon gives a model document that sets no icon its family's.
func (f *File) FillModelIcon(m *manifest.ModelDTO) {
	if m.Spec.Icon != nil && m.Spec.Icon.Path != "" {
		return
	}
	for _, fam := range f.Families {
		if fam.Name == m.Spec.Family && fam.Icon != nil && fam.Icon.Path != "" {
			m.Spec.Icon = &meta.Icon{Path: fam.Icon.Path}
			return
		}
	}
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
