// Package archive extracts a catalog release's data tarball with the limits
// relay and the catalog server share. Fetching and checksum checks stay with
// their callers.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxCatalogBytes caps the decompressed archive size. The real catalog is
// a few MB of YAML; the cap only exists so a mispointed URL (or a
// hostile mirror) can't fill the disk.
const maxCatalogBytes = 512 << 20

// maxCatalogEntries caps the archive's entry count; the real catalog has a
// few thousand files at most.
const maxCatalogEntries = 50_000

// Extract extracts a gzipped catalog tarball under destDir. Only regular
// files and directories are extracted; entries escaping destDir are rejected,
// and the content size and entry count are capped.
func Extract(r io.Reader, destDir string) error {
	return extractTarGz(r, destDir, maxCatalogBytes, maxCatalogEntries)
}

// extractTarGz writes at most maxBytes of file content (sparse holes count
// at their logical size) and at most maxEntries entries under destDir.
func extractTarGz(r io.Reader, destDir string, maxBytes int64, maxEntries int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	var written int64
	var entries int
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if entries++; entries > maxEntries {
			return fmt.Errorf("archive exceeds %d entries", maxEntries)
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
			// Compared against the remaining budget, never summed first:
			// a declared size near MaxInt64 would wrap the running total.
			remaining := maxBytes - written
			if hdr.Size < 0 || hdr.Size > remaining {
				return fmt.Errorf("archive exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, remaining))
			written += n
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
