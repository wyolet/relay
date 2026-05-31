package payload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// filePrefix marks URIs owned by FileStore. The remainder is the store-relative
// key, so URIs stay stable across process restarts as long as the root is.
const filePrefix = "file:"

// FileStore is a filesystem-backed Store. Blobs live under Root, one file per
// key (nested directories in the key are created as needed). It is the default
// zero-dependency backend, suitable for single-node and dev deployments.
type FileStore struct {
	root string
}

// NewFileStore returns a FileStore rooted at dir, creating it if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("payload: empty file store root")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("payload: create root: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("payload: resolve root: %w", err)
	}
	return &FileStore{root: abs}, nil
}

func (s *FileStore) Put(ctx context.Context, key string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	full, err := s.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("payload: mkdir: %w", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", fmt.Errorf("payload: write: %w", err)
	}
	return filePrefix + filepath.ToSlash(key), nil
}

func (s *FileStore) Get(ctx context.Context, uri string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := s.resolveURI(uri)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("payload: read: %w", err)
	}
	return b, nil
}

func (s *FileStore) Delete(ctx context.Context, uri string) error {
	full, err := s.resolveURI(uri)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("payload: delete: %w", err)
	}
	return nil
}

func (s *FileStore) resolveURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, filePrefix) {
		return "", fmt.Errorf("payload: not a file URI: %q", uri)
	}
	return s.resolve(strings.TrimPrefix(uri, filePrefix))
}

// resolve maps a key to an absolute path under root, rejecting any key that
// would escape root (path traversal).
func (s *FileStore) resolve(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	full := filepath.Join(s.root, clean)
	if full != s.root && !strings.HasPrefix(full, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("payload: key escapes root: %q", key)
	}
	return full, nil
}
