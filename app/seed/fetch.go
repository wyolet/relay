package seed

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCatalogURLTemplate is where versioned catalog trees are fetched
// from when RELAY_CATALOG_URL is unset. "{version}" is replaced with the
// requested ref; GitHub serves source archives for any tag/branch/sha.
const DefaultCatalogURLTemplate = "https://github.com/wyolet/relay-catalog/archive/{version}.tar.gz"

// maxCatalogBytes caps the decompressed archive size. The real catalog is
// a few MB of YAML; the cap only exists so a mispointed URL (or a
// hostile mirror) can't fill the disk.
const maxCatalogBytes = 512 << 20

// FetchCatalog downloads the catalog source archive for version from
// urlTemplate ("{version}" substituted; empty = DefaultCatalogURLTemplate),
// extracts it under destDir, and returns the directory holding the
// catalog data tree. Only regular files and directories are extracted —
// symlinks and entries escaping destDir are rejected.
//
// The returned root follows the source-archive convention: a top-level
// `data/` dir wins (directly under destDir, or inside a single wrapping
// directory the way GitHub archives unpack); otherwise destDir itself is
// assumed to be the data tree.
func FetchCatalog(ctx context.Context, urlTemplate, version, destDir string) (string, error) {
	if version == "" {
		return "", fmt.Errorf("seed: fetch catalog: version is required")
	}
	if urlTemplate == "" {
		urlTemplate = DefaultCatalogURLTemplate
	}
	url := strings.ReplaceAll(urlTemplate, "{version}", version)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("seed: fetch catalog: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("seed: fetch catalog %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("seed: fetch catalog %s: HTTP %d", url, resp.StatusCode)
	}
	if err := extractTarGz(resp.Body, destDir); err != nil {
		return "", fmt.Errorf("seed: extract catalog %s: %w", url, err)
	}
	return findDataRoot(destDir)
}

func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	var written int64
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := securePath(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if written += hdr.Size; written > maxCatalogBytes {
				return fmt.Errorf("archive exceeds %d bytes", int64(maxCatalogBytes))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, maxCatalogBytes))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			// Symlinks, hardlinks, devices: never present in a catalog
			// archive; skipping (not following) keeps extraction safe.
		}
	}
}

// securePath joins name under destDir and rejects entries that would
// escape it (path traversal via "..").
func securePath(destDir, name string) (string, error) {
	target := filepath.Join(destDir, filepath.FromSlash(name))
	if rel, err := filepath.Rel(destDir, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return target, nil
}

func findDataRoot(destDir string) (string, error) {
	if isDir(filepath.Join(destDir, "data")) {
		return filepath.Join(destDir, "data"), nil
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		nested := filepath.Join(destDir, entries[0].Name(), "data")
		if isDir(nested) {
			return nested, nil
		}
	}
	return destDir, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
