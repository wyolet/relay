package seed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/manifest"
)

// DefaultCatalogIndexURL is where the channel index is fetched from when
// RELAY_CATALOG_INDEX_URL is unset. The index maps schema channels to the
// newest catalog release published for them, so a relay only ever
// resolves "latest" within the apiVersion it can parse.
const DefaultCatalogIndexURL = "https://raw.githubusercontent.com/wyolet/relay-catalog/main/index.yaml"

// versionFile is the stamp catalog-embedding images write next to the
// bundled data tree (Dockerfile writes CATALOG_REF into it). Its presence
// makes a local tree self-identifying so a matching version pin can seed
// from disk instead of fetching.
const versionFile = ".version"

// IsLatestAlias reports whether v selects channel-latest resolution
// instead of naming a concrete catalog release.
func IsLatestAlias(v string) bool { return v == "latest" || v == "auto" }

// Channel is the schema channel key this binary resolves "latest"
// against: the version segment of the manifest apiVersion ("v1alpha2").
func Channel() string { return path.Base(manifest.APIVersion) }

// DirVersion returns the release stamp of a local catalog tree (the
// .version file next to the data), or "" when the tree is unstamped.
func DirVersion(dir string) string {
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, versionFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type catalogIndex struct {
	Channels map[string]struct {
		Latest string `yaml:"latest"`
	} `yaml:"channels"`
}

// ResolveLatest fetches the channel index (indexURL, empty =
// DefaultCatalogIndexURL) and returns the newest catalog release for
// channel. A channel absent from the index is an error — it means no
// catalog release exists for this binary's schema version.
func ResolveLatest(ctx context.Context, indexURL, channel string) (string, error) {
	if indexURL == "" {
		indexURL = DefaultCatalogIndexURL
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return "", fmt.Errorf("seed: catalog index: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("seed: catalog index %s: %w", indexURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("seed: catalog index %s: HTTP %d", indexURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("seed: catalog index %s: %w", indexURL, err)
	}
	var idx catalogIndex
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return "", fmt.Errorf("seed: catalog index %s: %w", indexURL, err)
	}
	ch, ok := idx.Channels[channel]
	if !ok || ch.Latest == "" {
		return "", fmt.Errorf("seed: catalog index %s: no release for channel %q", indexURL, channel)
	}
	return ch.Latest, nil
}
