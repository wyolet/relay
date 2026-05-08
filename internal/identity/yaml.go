package identity

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Store holds the loaded User set. It is intentionally small — the catalog
// snapshot pattern is overkill until identity actually has cross-entity
// references to validate.
type Store struct {
	users map[string]*User
}

func (s *Store) Users() []*User {
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *Store) ByName(name string) (*User, bool) {
	u, ok := s.users[name]
	return u, ok
}

// LoadYAML walks dir and parses every .yaml/.yml file, picking up only
// kind=User documents. Other kinds are silently skipped so this can run over
// the same tree the catalog loader walks.
func LoadYAML(dir string) (*Store, error) {
	store := &Store{users: map[string]*User{}}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		return loadFile(path, store)
	})
	if err != nil {
		return nil, err
	}

	if err := resolveAll(store); err != nil {
		return nil, err
	}
	if err := validate(store); err != nil {
		return nil, err
	}
	return store, nil
}

type rawDoc struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

func loadFile(path string, store *Store) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	idx := 0
	for {
		var raw rawDoc
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s [doc %d]: %w", path, idx, err)
		}
		idx++
		if raw.Kind != KindUser {
			continue
		}
		if raw.APIVersion != APIVersion {
			return fmt.Errorf("%s: User %q: unsupported apiVersion %q (want %q)",
				path, raw.Metadata.Name, raw.APIVersion, APIVersion)
		}
		if raw.Metadata.Name == "" {
			return fmt.Errorf("%s: User: metadata.name required", path)
		}
		var spec UserSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s: User %q: %w", path, raw.Metadata.Name, err)
		}
		if _, dup := store.users[raw.Metadata.Name]; dup {
			return fmt.Errorf("%s: duplicate User %q", path, raw.Metadata.Name)
		}
		store.users[raw.Metadata.Name] = &User{
			APIVersion: raw.APIVersion,
			Kind:       raw.Kind,
			Metadata:   raw.Metadata,
			Spec:       spec,
		}
	}
	return nil
}

func resolveAll(store *Store) error {
	var literalUsers []string
	for name, u := range store.users {
		fields := []struct {
			path string
			ref  *SecretRef
		}{
			{fmt.Sprintf("User %q.spec.username", name), &u.Spec.Username},
			{fmt.Sprintf("User %q.spec.email", name), &u.Spec.Email},
			{fmt.Sprintf("User %q.spec.password", name), &u.Spec.Password},
		}
		for _, f := range fields {
			if err := f.ref.Resolve(f.path); err != nil {
				return err
			}
		}
		if u.Spec.Password.IsLiteral() {
			literalUsers = append(literalUsers, name)
		}
	}
	if len(literalUsers) > 0 {
		sort.Strings(literalUsers)
		slog.Warn("identity: users with inline password literal in YAML — prefer valueFrom.env or valueFrom.file",
			"users", literalUsers)
	}
	return nil
}

func validate(store *Store) error {
	for name, u := range store.users {
		if _, err := mail.ParseAddress(u.Spec.Email.Get()); err != nil {
			return fmt.Errorf("User %q: invalid email: %w", name, err)
		}
		if len(u.Spec.Username.Get()) < 1 {
			return fmt.Errorf("User %q: username empty after resolution", name)
		}
		if len(u.Spec.Password.Get()) < 8 {
			return fmt.Errorf("User %q: password must be at least 8 characters", name)
		}
	}
	return nil
}
