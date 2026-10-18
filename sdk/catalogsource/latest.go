package catalogsource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/internal/atomicfile"
)

const (
	// EnvCatalogURL is the base URL of a catalog server that Latest reads; "off" serves the embedded catalog with no network access.
	EnvCatalogURL = "WR_CATALOG_URL"
	// EnvCatalogCacheDir turns on Latest's disk cache in that directory.
	EnvCatalogCacheDir = "WR_CATALOG_CACHE_DIR"

	// DefaultRefreshInterval is how long Latest serves a result before it checks the index again.
	DefaultRefreshInterval = 10 * time.Minute

	catalogURLOff = "off"
	// lastTagFile is per channel, so SDKs of different schemas can share a cache dir.
	lastTagFile = "latest-" + Channel
)

// LatestOption configures Latest.
type LatestOption func(*LatestSource)

// WithBaseURL points Latest at a catalog server: the index at base/index.yaml, releases under base/releases. "off" serves the embedded catalog with no network access.
func WithBaseURL(base string) LatestOption {
	return func(s *LatestSource) {
		if base == catalogURLOff {
			s.off = true
			return
		}
		base = strings.TrimRight(base, "/")
		s.off = false
		s.release.indexURL = base + "/index.yaml"
		s.release.downloadURL = base + "/releases"
	}
}

// WithLatestIndexURL replaces DefaultIndexURL.
func WithLatestIndexURL(url string) LatestOption {
	return func(s *LatestSource) {
		s.off = false
		s.release.indexURL = url
	}
}

// WithLatestDownloadURL replaces DefaultDownloadURL.
func WithLatestDownloadURL(base string) LatestOption {
	return func(s *LatestSource) {
		s.off = false
		s.release.downloadURL = strings.TrimRight(base, "/")
	}
}

// WithCacheDir caches release assets and the last resolved tag in dir. "" turns the disk cache off.
func WithCacheDir(dir string) LatestOption {
	return func(s *LatestSource) { s.release.cacheDir = dir }
}

// WithLatestHTTPClient replaces the default client (a plain client with a 60s timeout).
func WithLatestHTTPClient(c *http.Client) LatestOption {
	return func(s *LatestSource) {
		if c != nil {
			s.release.client = c
		}
	}
}

// WithRefreshInterval replaces DefaultRefreshInterval; 0 checks the index on every call.
func WithRefreshInterval(d time.Duration) LatestOption {
	return func(s *LatestSource) { s.refresh = d }
}

// LatestResult is one Fetch.
type LatestResult struct {
	Catalog *catalog.IndexedCatalog
	// Tag is the release tag Catalog was built from; "" when the embed records none.
	Tag string
	// Embedded marks the catalog compiled into this SDK, served because the network is off, the server's latest is older, or nothing else could be read.
	Embedded bool
	// Stale marks an earlier fetched copy, served because a refresh failed.
	Stale bool
	// Err is the cause behind Stale or an embedded fallback; nil when the catalog is current.
	Err error
}

// LatestSource reads the newest catalog release for Channel. Build it with Latest.
type LatestSource struct {
	release releaseSource
	off     bool
	refresh time.Duration
	now     func() time.Time

	mu        sync.Mutex
	embed     *catalog.IndexedCatalog
	last      *LatestResult
	checked   time.Time
	indexETag string
	indexTag  string
	heldTag   string
	held      *catalog.IndexedCatalog
}

// Latest is the newest catalog release for Channel, read from DefaultIndexURL and DefaultDownloadURL. WR_CATALOG_URL and WR_CATALOG_CACHE_DIR are read here, and opts override them; a URL option overrides "off". Nothing is fetched until the first Fetch or Catalog, and the disk is written only with a cache dir. A release older than the embedded catalog is never served.
func Latest(opts ...LatestOption) *LatestSource {
	s := &LatestSource{
		release: releaseSource{
			indexURL:    DefaultIndexURL,
			downloadURL: DefaultDownloadURL,
			client:      &http.Client{Timeout: defaultTimeout},
		},
		refresh: DefaultRefreshInterval,
		now:     time.Now,
	}
	if base := os.Getenv(EnvCatalogURL); base != "" {
		WithBaseURL(base)(s)
	}
	s.release.cacheDir = os.Getenv(EnvCatalogCacheDir)
	for _, o := range opts {
		o(s)
	}
	return s
}

// Catalog returns Fetch's catalog, so a LatestSource is a Source.
func (s *LatestSource) Catalog(ctx context.Context) (*catalog.IndexedCatalog, error) {
	res, err := s.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	return res.Catalog, nil
}

// Fetch returns the newest release, checking the index at most once per refresh interval. When the index or the release can't be read, it serves, with Err set: the copy held in memory (Stale), else the last tag in the disk cache (Stale), else the embed (Embedded). The error is non-nil only when the embed fails to load.
func (s *LatestSource) Fetch(ctx context.Context) (*LatestResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.off {
		return s.embedded(nil)
	}
	now := s.now()
	if s.last != nil && now.Sub(s.checked) < s.refresh {
		res := *s.last
		return &res, nil
	}
	res, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	// A failed check is kept for the interval too, so a down server is retried once per interval; the caller's own cancellation says nothing about the server.
	if ctx.Err() == nil {
		s.checked, s.last = now, res
	}
	out := *res
	return &out, nil
}

func (s *LatestSource) check(ctx context.Context) (*LatestResult, error) {
	tag, err := s.indexLatest(ctx)
	if err != nil {
		return s.fallback(err)
	}
	if olderRelease(tag, EmbeddedVersion()) {
		return s.embedded(nil)
	}
	if tag == s.heldTag && s.held != nil {
		return &LatestResult{Catalog: s.held, Tag: tag}, nil
	}
	ic, err := s.release.catalogForTag(ctx, tag)
	if err != nil {
		return s.fallback(err)
	}
	s.heldTag, s.held = tag, ic
	s.rememberTag(tag)
	return &LatestResult{Catalog: ic, Tag: tag}, nil
}

// indexLatest is Channel's latest tag, revalidating the index against the ETag of its last read.
func (s *LatestSource) indexLatest(ctx context.Context) (string, error) {
	body, etag, notModified, err := s.release.getIfNoneMatch(ctx, s.release.indexURL, maxSmallBytes, s.indexETag)
	if err != nil {
		return "", fmt.Errorf("catalogsource: latest release: %w", err)
	}
	if notModified {
		return s.indexTag, nil
	}
	tag, err := s.release.latestTag(body)
	if err != nil {
		return "", fmt.Errorf("catalogsource: latest release: %w", err)
	}
	s.indexETag, s.indexTag = etag, tag
	return tag, nil
}

func (s *LatestSource) fallback(cause error) (*LatestResult, error) {
	if s.held == nil {
		s.heldTag, s.held = s.diskCatalog()
	}
	if s.held != nil {
		return &LatestResult{Catalog: s.held, Tag: s.heldTag, Stale: true, Err: cause}, nil
	}
	return s.embedded(cause)
}

// diskCatalog is the last resolved tag's cached catalog. Anything missing or unreadable yields nothing, and the caller falls back to the embed.
func (s *LatestSource) diskCatalog() (string, *catalog.IndexedCatalog) {
	if s.release.cacheDir == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(s.release.cacheDir, lastTagFile))
	if err != nil {
		return "", nil
	}
	tag := strings.TrimSpace(string(data))
	if checkTag(tag) != nil || olderRelease(tag, EmbeddedVersion()) {
		return "", nil
	}
	ic, err := s.release.loadCached(tag)
	if err != nil {
		return "", nil
	}
	return tag, ic
}

// rememberTag records tag for a restart that finds the server down. A failed write only costs that fallback, so it isn't reported.
func (s *LatestSource) rememberTag(tag string) {
	if s.release.cacheDir == "" {
		return
	}
	if err := os.MkdirAll(s.release.cacheDir, 0o755); err != nil {
		return
	}
	_ = atomicfile.Write(filepath.Join(s.release.cacheDir, lastTagFile), []byte(tag+"\n"), 0o644)
}

func (s *LatestSource) embedded(cause error) (*LatestResult, error) {
	if s.embed == nil {
		ic, err := loadEmbedded()
		if err != nil {
			return nil, errors.Join(cause, fmt.Errorf("catalogsource: embedded catalog: %w", err))
		}
		s.embed = ic
	}
	return &LatestResult{Catalog: s.embed, Tag: EmbeddedVersion(), Embedded: true, Err: cause}, nil
}

// olderRelease reports whether tag is a lower vX.Y.Z than base. A tag of any other form is never compared.
func olderRelease(tag, base string) bool {
	a, ok := parseRelease(tag)
	if !ok {
		return false
	}
	b, ok := parseRelease(base)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parseRelease(tag string) ([3]int, bool) {
	var v [3]int
	rest, ok := strings.CutPrefix(tag, "v")
	if !ok {
		return v, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != len(v) {
		return v, false
	}
	for i, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return v, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
