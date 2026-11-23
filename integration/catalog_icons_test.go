//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/catalogicon"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// The stored icons are those of the last seeded tree that has an icons
// directory; a tree without one, such as a config directory, leaves them.
func TestIntegration_SeedStoresTheTreesIcons(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := storagetest.Pool(t)
	store := catalogicon.NewStore(pool)

	seedTree := func(icons map[string]string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(detachCatalog), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, body := range icons {
			p := filepath.Join(dir, catalogicon.Dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir, CatalogKindsOnly: true}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	stored := func(name string) string {
		t.Helper()
		ic, err := store.Get(ctx, name)
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(ic.Body)
	}

	seedTree(map[string]string{"acme.png": "first", "old.png": "old"})
	if got := stored("acme.png"); got != "first" {
		t.Fatalf("acme.png = %q after the first seed", got)
	}
	seedTree(map[string]string{"acme.png": "second"})
	if got, old := stored("acme.png"), stored("old.png"); got != "second" || old != "" {
		t.Fatalf("after a release that redrew one icon and dropped another: acme.png %q, old.png %q", got, old)
	}
	seedTree(nil)
	if got := stored("acme.png"); got != "second" {
		t.Fatalf("acme.png = %q after seeding a tree without icons", got)
	}
}
