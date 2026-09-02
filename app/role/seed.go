package role

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
)

//go:embed builtin.yaml
var builtinYAML []byte

// builtinDoc is the slice of the manifest envelope the built-ins need. The
// file is parsed here rather than through app/manifest because manifest
// translates into this package.
type builtinDoc struct {
	Metadata struct {
		Name        string `yaml:"name"`
		DisplayName string `yaml:"displayName"`
		Description string `yaml:"description"`
	} `yaml:"metadata"`
	Spec Spec `yaml:"spec"`
}

// Builtins returns the built-in system Roles, freshly parsed with no ids
// stamped. Order follows the embedded file.
func Builtins() ([]*Role, error) {
	dec := yaml.NewDecoder(bytes.NewReader(builtinYAML))
	var out []*Role
	for {
		var d builtinDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("role: parse builtin.yaml: %w", err)
		}
		out = append(out, &Role{
			Meta: meta.Metadata{
				Name:        d.Metadata.Name,
				DisplayName: d.Metadata.DisplayName,
				Description: d.Metadata.Description,
				Owner:       meta.Owner{Kind: meta.OwnerSystem},
			},
			Spec: d.Spec,
		})
	}
	return out, nil
}

// IsBuiltin reports whether name belongs to a built-in role. Built-in names
// are reserved: a custom Role may not claim one.
func IsBuiltin(name string) bool {
	roles, err := Builtins()
	if err != nil {
		return false
	}
	for _, r := range roles {
		if r.Meta.Name == name {
			return true
		}
	}
	return false
}

// SeedBuiltins writes the built-in Roles that are not in the table yet,
// seed-if-absent by name — same pattern as the user seed. Existing rows are
// left alone so an operator's edits survive a restart.
func SeedBuiltins(ctx context.Context, s *Store, log *slog.Logger) error {
	if s == nil {
		return nil
	}
	existing, err := s.List(ctx)
	if err != nil {
		return fmt.Errorf("role seed: list: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		have[r.Meta.Name] = struct{}{}
	}
	builtins, err := Builtins()
	if err != nil {
		return err
	}
	seeded := 0
	for _, r := range builtins {
		if _, ok := have[r.Meta.Name]; ok {
			continue
		}
		r.Meta.ID = ids.New()
		if err := r.Validate(); err != nil {
			return fmt.Errorf("role seed: %q: %w", r.Meta.Name, err)
		}
		if err := s.Upsert(ctx, r); err != nil {
			return fmt.Errorf("role seed: upsert %q: %w", r.Meta.Name, err)
		}
		seeded++
	}
	if seeded > 0 && log != nil {
		log.Info("built-in roles seeded", "count", seeded)
	}
	return nil
}
