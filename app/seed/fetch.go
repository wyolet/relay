package seed

import (
	"bytes"
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

	catalogarchive "github.com/wyolet/relay/pkg/catalogkind/archive"
)

// DefaultCatalogURLTemplate is where versioned catalog trees are fetched
// from when RELAY_CATALOG_URL is unset. "{version}" is replaced with the
// requested release tag.
const DefaultCatalogURLTemplate = "https://catalog.wyolet.com/releases/{version}/relay-catalog-data-{version}.tar.gz"

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
	if err := catalogarchive.Extract(bytes.NewReader(archive), destDir); err != nil {
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
