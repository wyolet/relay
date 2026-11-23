// Package catalogicon keeps the icon files of the seeded catalog release in
// Postgres and reads them back by name.
//
// A catalog entity's icon path "/provider/<name>" names a file in the
// release's icons/ directory. The seed copies that directory into Postgres,
// so a pod that never saw the release tarball serves the same icons as the
// pod that fetched it.
//
// Out of scope: resizing or converting images, and icons an operator
// uploads. The set is the catalog release's, replaced as a whole.
package catalogicon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
)

// Dir is the icons directory of a catalog data tree.
const Dir = "icons"

// MaxBytes bounds one icon; a larger file in a tree is skipped.
const MaxBytes = 1 << 20

var fileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

var contentTypes = map[string]string{
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// ContentType is the media type of an icon file name; ok is false for a name
// that is not a plain image file name.
func ContentType(name string) (contentType string, ok bool) {
	if !fileName.MatchString(name) {
		return "", false
	}
	contentType, ok = contentTypes[strings.ToLower(path.Ext(name))]
	return contentType, ok
}

// Icon is one icon file.
type Icon struct {
	Name   string
	Body   []byte
	SHA256 string
}

// New returns the icon named name holding body.
func New(name string, body []byte) Icon {
	sum := sha256.Sum256(body)
	return Icon{Name: name, Body: body, SHA256: hex.EncodeToString(sum[:])}
}

// ReadFile reads one icon of the data tree at dataDir, or returns
// fs.ErrNotExist for a name that is no icon there.
func ReadFile(dataDir, name string) (Icon, error) {
	if _, ok := ContentType(name); !ok {
		return Icon{}, fs.ErrNotExist
	}
	p := filepath.Join(dataDir, Dir, name)
	info, err := os.Stat(p)
	if err != nil {
		return Icon{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return Icon{}, fs.ErrNotExist
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return Icon{}, err
	}
	return New(name, body), nil
}

// ReadDir reads every icon of the data tree at dataDir. found is false when
// the tree has no icons directory.
func ReadDir(dataDir string) (icons []Icon, found bool, err error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		ic, err := ReadFile(dataDir, e.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		icons = append(icons, ic)
	}
	return icons, true, nil
}

// Store is the data-access layer for the seeded icons.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Get returns the icon named name, or fs.ErrNotExist.
func (s *Store) Get(ctx context.Context, name string) (Icon, error) {
	r, err := gen.New(s.pool).GetCatalogIcon(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Icon{}, fs.ErrNotExist
	}
	if err != nil {
		return Icon{}, fmt.Errorf("catalog icon %s: %w", name, err)
	}
	return Icon{Name: name, Body: r.Body, SHA256: r.Sha256}, nil
}

// Replace makes icons the whole stored set, in one transaction.
func (s *Store) Replace(ctx context.Context, icons []Icon) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := gen.New(tx)
		names := make([]string, 0, len(icons))
		for _, ic := range icons {
			names = append(names, ic.Name)
			if err := q.UpsertCatalogIcon(ctx, gen.UpsertCatalogIconParams{Name: ic.Name, Body: ic.Body, Sha256: ic.SHA256}); err != nil {
				return fmt.Errorf("catalog icon %s: %w", ic.Name, err)
			}
		}
		return q.DeleteCatalogIconsExcept(ctx, names)
	})
}
