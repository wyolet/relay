package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// requested release tag.
const DefaultCatalogURLTemplate = "https://catalog.wyolet.com/releases/{version}/relay-catalog-data-{version}.tar.gz"

// maxCatalogBytes caps the decompressed archive size. The real catalog is
// a few MB of YAML; the cap only exists so a mispointed URL (or a
// hostile mirror) can't fill the disk.
const maxCatalogBytes = 512 << 20

// maxCatalogEntries caps the archive's entry count; the real catalog has a
// few thousand files at most.
const maxCatalogEntries = 50_000

// maxArchiveBytes caps the compressed download, which is held in memory until
// its sha256 is checked. A release's data tarball is about 100 KB.
const maxArchiveBytes = 64 << 20

// maxSumBytes caps the .sha256 response: one "<hex>  <name>" line.
const maxSumBytes = 4 << 10

// FetchCatalog downloads the catalog archive for version from urlTemplate
// ("{version}" substituted; empty = DefaultCatalogURLTemplate), checks it
// against the sha256sum file at the same URL plus ".sha256", extracts it
// under destDir, and returns the directory holding the catalog data tree.
// A missing, malformed or mismatching sum is an error and nothing is
// extracted. Only regular files and directories are extracted — symlinks
// and entries escaping destDir are rejected.
//
// A top-level `data/` dir is the data root (directly under destDir, or
// inside a single wrapping directory); otherwise destDir itself is assumed
// to be the data tree.
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
	sumBody, err := httpGet(ctx, url+".sha256", maxSumBytes)
	if err != nil {
		return "", fmt.Errorf("seed: fetch catalog checksum: %w", err)
	}
	want, err := parseSHA256Sum(sumBody)
	if err != nil {
		return "", fmt.Errorf("seed: catalog checksum %s.sha256: %w", url, err)
	}
	archive, err := httpGet(ctx, url, maxArchiveBytes)
	if err != nil {
		return "", fmt.Errorf("seed: fetch catalog: %w", err)
	}
	if got := sha256.Sum256(archive); got != want {
		return "", fmt.Errorf("seed: catalog %s: sha256 %x does not match the published %x", url, got, want)
	}
	if err := extractTarGz(bytes.NewReader(archive), destDir, maxCatalogBytes, maxCatalogEntries); err != nil {
		return "", fmt.Errorf("seed: extract catalog %s: %w", url, err)
	}
	return findDataRoot(destDir)
}

// httpGet returns the body of a 200 response, failing when it exceeds maxBytes.
func httpGet(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", url, maxBytes)
	}
	return body, nil
}

// parseSHA256Sum reads the digest from sha256sum output ("<hex>  <name>").
func parseSHA256Sum(b []byte) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	fields := strings.Fields(string(b))
	if len(fields) == 0 || len(fields[0]) != hex.EncodedLen(sha256.Size) {
		return sum, fmt.Errorf("not sha256sum output")
	}
	if _, err := hex.Decode(sum[:], []byte(fields[0])); err != nil {
		return sum, fmt.Errorf("not sha256sum output")
	}
	return sum, nil
}

// ExtractCatalogArchive extracts a gzipped catalog tarball under destDir
// with FetchCatalog's size, entry-count and path limits.
func ExtractCatalogArchive(r io.Reader, destDir string) error {
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
