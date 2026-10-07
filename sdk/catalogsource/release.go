package catalogsource

import (
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

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/internal/atomicfile"
)

const (
	// Channel is the catalog schema channel this SDK reads. "Latest" resolves within it, so a schema bump never hands this SDK a catalog it cannot parse.
	Channel = "v1alpha2"
	// DefaultIndexURL maps each schema channel to its newest catalog release.
	DefaultIndexURL = "https://raw.githubusercontent.com/wyolet/relay-catalog/main/index.yaml"
	// DefaultDownloadURL is the release download base; assets live at <base>/<tag>/<asset>.
	DefaultDownloadURL = "https://github.com/wyolet/relay-catalog/releases/download"

	assetName      = "catalog.json.gz"
	maxAssetBytes  = 64 << 20
	maxSmallBytes  = 1 << 20
	defaultTimeout = 60 * time.Second
)

// ReleaseOption configures Release.
type ReleaseOption func(*releaseSource)

// WithIndexURL replaces DefaultIndexURL.
func WithIndexURL(url string) ReleaseOption {
	return func(r *releaseSource) { r.indexURL = url }
}

// WithDownloadURL replaces DefaultDownloadURL.
func WithDownloadURL(base string) ReleaseOption {
	return func(r *releaseSource) { r.downloadURL = strings.TrimRight(base, "/") }
}

// WithHTTPClient replaces the default client (a plain client with a 60s timeout).
func WithHTTPClient(c *http.Client) ReleaseOption {
	return func(r *releaseSource) {
		if c != nil {
			r.client = c
		}
	}
}

type releaseSource struct {
	tag         string
	cacheDir    string
	indexURL    string
	downloadURL string
	client      *http.Client
}

// Release is a relay-catalog GitHub release asset. tag "" resolves the newest release for Channel through the index on every call. A tag equal to EmbeddedVersion is served from the embedded catalog without fetching. Otherwise the asset is checked against its published sha256 and cached under cacheDir/<tag>; a cached tag is never fetched again because release tags are immutable. cacheDir "" disables the cache. The returned catalog's Version is the tag.
func Release(tag, cacheDir string, opts ...ReleaseOption) Source {
	r := &releaseSource{
		tag:         tag,
		cacheDir:    cacheDir,
		indexURL:    DefaultIndexURL,
		downloadURL: DefaultDownloadURL,
		client:      &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *releaseSource) Catalog(ctx context.Context) (*catalog.IndexedCatalog, error) {
	tag := r.tag
	if tag == "" {
		latest, err := r.latestTag(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalogsource: latest release: %w", err)
		}
		tag = latest
	}
	if err := checkTag(tag); err != nil {
		return nil, err
	}
	if tag == EmbeddedVersion() {
		ic, err := loadEmbedded()
		if err != nil {
			return nil, fmt.Errorf("catalogsource: release %s (embedded): %w", tag, err)
		}
		return ic, nil
	}
	cached := ""
	if r.cacheDir != "" {
		cached = filepath.Join(r.cacheDir, tag, assetName)
		if _, err := os.Stat(cached); err == nil {
			ic, err := catalog.LoadFile(cached)
			if err != nil {
				return nil, fmt.Errorf("catalogsource: cached release %s: %w", tag, err)
			}
			ic.Catalog.Version = tag
			return ic, nil
		}
	}
	ic, asset, err := r.download(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("catalogsource: release %s: %w", tag, err)
	}
	if cached != "" {
		if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
			return nil, fmt.Errorf("catalogsource: cache: %w", err)
		}
		if err := atomicfile.Write(cached, asset, 0o644); err != nil {
			return nil, fmt.Errorf("catalogsource: cache: %w", err)
		}
	}
	ic.Catalog.Version = tag
	return ic, nil
}

// download fetches and verifies the asset and parses it before anything is cached, so a bad asset never lands in the cache.
func (r *releaseSource) download(ctx context.Context, tag string) (*catalog.IndexedCatalog, []byte, error) {
	assetURL := r.downloadURL + "/" + tag + "/" + assetName
	asset, err := r.get(ctx, assetURL, maxAssetBytes)
	if err != nil {
		return nil, nil, err
	}
	sumFile, err := r.get(ctx, assetURL+".sha256", maxSmallBytes)
	if err != nil {
		return nil, nil, err
	}
	fields := strings.Fields(string(sumFile))
	if len(fields) == 0 {
		return nil, nil, fmt.Errorf("%s.sha256: empty", assetURL)
	}
	sum := sha256.Sum256(asset)
	if !strings.EqualFold(fields[0], hex.EncodeToString(sum[:])) {
		return nil, nil, fmt.Errorf("%s: sha256 mismatch", assetURL)
	}
	gz, err := gzip.NewReader(bytes.NewReader(asset))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", assetURL, err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", assetURL, err)
	}
	ic, err := catalog.LoadBytes(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", assetURL, err)
	}
	return ic, asset, nil
}

type channelIndex struct {
	Channels map[string]struct {
		Latest string `yaml:"latest"`
	} `yaml:"channels"`
}

func (r *releaseSource) latestTag(ctx context.Context) (string, error) {
	body, err := r.get(ctx, r.indexURL, maxSmallBytes)
	if err != nil {
		return "", err
	}
	var idx channelIndex
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return "", fmt.Errorf("%s: %w", r.indexURL, err)
	}
	ch, ok := idx.Channels[Channel]
	if !ok || ch.Latest == "" {
		return "", fmt.Errorf("%s: no release for channel %q", r.indexURL, Channel)
	}
	return ch.Latest, nil
}

func (r *releaseSource) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return body, nil
}

// checkTag keeps a tag, which may come from the fetched index, a single path segment in URLs and the cache.
func checkTag(tag string) error {
	if tag == "" || strings.HasPrefix(tag, ".") || strings.ContainsAny(tag, `/\?#%`) {
		return fmt.Errorf("catalogsource: invalid catalog tag %q", tag)
	}
	return nil
}
