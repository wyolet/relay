package modeldir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
)

const fileExt = ".yaml"

// ErrNoModels reports a model directory that is missing or holds no model files. Load still returns an empty, usable catalog alongside it.
var ErrNoModels = errors.New("no model files")

// Load reads every model file in dir into a catalog holding only those models. A missing or empty dir yields an empty catalog and an error wrapping ErrNoModels; an unreadable or invalid file yields a nil catalog.
func Load(dir string) (*catalog.IndexedCatalog, error) {
	models, err := List(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ic, indexErr := catalog.Index(buildCatalog(models))
	if indexErr != nil {
		return nil, indexErr
	}
	if err != nil {
		return ic, fmt.Errorf("modeldir: %s: %w: %w", dir, ErrNoModels, fs.ErrNotExist)
	}
	if len(models) == 0 {
		return ic, fmt.Errorf("modeldir: %s: %w", dir, ErrNoModels)
	}
	return ic, nil
}

// List returns the model files in dir sorted by name. A missing dir is an error wrapping fs.ErrNotExist.
func List(dir string) ([]Model, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("modeldir: %w", err)
	}
	var models []Model
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), fileExt)
		if !ok || e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		m, err := readModelFile(dir, name)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, nil
}

// Remove deletes the model file for name and nothing else.
func Remove(dir, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := os.Remove(modelPath(dir, name)); err != nil {
		return fmt.Errorf("modeldir: %w", err)
	}
	return nil
}

func modelPath(dir, name string) string {
	return filepath.Join(dir, name+fileExt)
}

// checkName keeps a model name a single plain file name inside the directory.
func checkName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("modeldir: invalid model name %q", name)
	}
	return nil
}

func readModelFile(dir, name string) (Model, error) {
	path := modelPath(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Model{}, fmt.Errorf("modeldir: %w", err)
	}
	m, err := decodeModel(data)
	if err != nil {
		return Model{}, fmt.Errorf("modeldir: %s: %w", path, err)
	}
	if m.Name != name {
		return Model{}, fmt.Errorf("modeldir: %s: name %q does not match the file name", path, m.Name)
	}
	return m, nil
}

// writeModelFile replaces the file in one rename so a reader never sees a partial file.
func writeModelFile(dir string, m Model) error {
	if err := checkName(m.Name); err != nil {
		return err
	}
	data, err := encodeModel(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(modelPath(dir, m.Name), data)
}

func writeFileAtomic(path string, data []byte) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return fmt.Errorf("modeldir: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("modeldir: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("modeldir: write %s: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("modeldir: write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("modeldir: write %s: %w", path, err)
	}
	return nil
}
