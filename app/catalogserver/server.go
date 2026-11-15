package catalogserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
)

const (
	// CI publishes a tag's files a minute or two after the push.
	freshTagAge   = 24 * time.Hour
	staleSumRetry = time.Hour
	failureRetry  = 5 * time.Minute
	extraFetchGap = 10 * time.Second
	maxBuilds     = 2
	buildTime     = 2 * time.Minute
	sumFetchers   = 4
)

var errUnavailable = errors.New("unavailable")

type asset int

const (
	assetCatalog asset = iota
	assetData
	assetMCP
	assetCount
)

var allAssets = [...]asset{assetCatalog, assetData, assetMCP}

// indexedFiles is what one channel index requires of a tag: its files, and
// the channel it files the tag under ("" keeps the tag out).
type indexedFiles struct {
	assets  []asset
	channel func(*tagState) string
}

var (
	// relayFiles back /index.yaml, which relays and the SDK read.
	relayFiles = indexedFiles{
		assets:  []asset{assetCatalog, assetData},
		channel: func(st *tagState) string { return st.channel },
	}
	// mcpFiles back /mcp/index.yaml, a separate index so no relay or SDK
	// client ever sees its channels.
	mcpFiles = indexedFiles{
		assets:  []asset{assetMCP},
		channel: func(st *tagState) string { return st.mcpChannel },
	}
)

func (a asset) label() string {
	switch a {
	case assetData:
		return "data"
	case assetMCP:
		return "mcp"
	}
	return "catalog"
}

func (a asset) fileName(tag string) string {
	switch a {
	case assetData:
		return DataAsset(tag)
	case assetMCP:
		return MCPAsset(tag)
	}
	return CatalogAsset
}

func assetNamed(tag, name string) (asset, bool) {
	switch name {
	case CatalogAsset:
		return assetCatalog, true
	case DataAsset(tag):
		return assetData, true
	case MCPAsset(tag):
		return assetMCP, true
	}
	return 0, false
}

// isReleaseTag accepts exactly vX.Y.Z, which also keeps a tag a safe path
// segment.
func isReleaseTag(tag string) bool {
	return semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == ""
}

type publishedSum struct {
	file      []byte // served verbatim
	sum       string // "" until published
	checkedAt time.Time
}

type verifiedFile struct {
	// The rebuild differed or failed, so the published file is what's served.
	fromRelease bool
}

type tagState struct {
	Tag
	channel    string // "" when the tree has no single apiVersion
	hasMCP     bool   // the tree has an mcp/ directory
	mcpChannel string // "" without one, or when mcp/ has no single apiVersion
	moved      bool
	sums       [assetCount]publishedSum
	verified   [assetCount]*verifiedFile
	failedAt   [assetCount]time.Time
}

// expects reports whether the tag's release can have a's file at all; a
// tree without mcp/ never publishes the MCP tarball, so its sum isn't asked for.
func (t *tagState) expects(a asset) bool {
	return a != assetMCP || t.hasMCP
}

func (t *tagState) published(f indexedFiles) bool {
	for _, a := range f.assets {
		if t.sums[a].sum == "" {
			return false
		}
	}
	return true
}

// Config configures a Server.
type Config struct {
	RepoURL      string
	ReleasesURL  string // files of tag live at <ReleasesURL>/<tag>/<name>
	MirrorDir    string
	SyncInterval time.Duration
	CacheBytes   int64
	HTTPClient   *http.Client // nil: a client with a one-minute timeout
	Logger       *slog.Logger // nil: slog.Default()

	// DisableWebhooks turns off receiver registration, release nudges and
	// the repository push hook.
	DisableWebhooks bool
	// AllowLoopbackWebhooks admits http:// receivers on loopback addresses.
	// For local development only; every other refused range stays refused.
	AllowLoopbackWebhooks bool
	// TrustedProxies are the peers whose X-Forwarded-For names the client
	// a registration is rate-limited by.
	TrustedProxies []*net.IPNet
	Version        string // sent in the User-Agent to receivers
}

// Server mirrors the catalog repository and serves its releases.
type Server struct {
	cfg      Config
	mirror   *Mirror
	releases releases
	log      *slog.Logger
	metrics  *metrics
	cache    *fileCache
	builds   singleflight.Group
	slots    chan struct{}
	now      func() time.Time
	fetchNow chan struct{}
	syncReqs syncRequests
	running  atomic.Bool
	syncMu   sync.Mutex // one sync at a time, whatever asked for it

	hooks      *webhooks // nil when webhooks are disabled
	deliveries sync.WaitGroup

	mu        sync.RWMutex
	tags      map[string]*tagState
	latest    map[string]string // channel → tag
	index     []byte
	indexETag string
	synced    bool

	mcpLatest    map[string]string // channel → tag, for /mcp/index.yaml
	mcpIndex     []byte
	mcpIndexETag string
}

// New validates cfg and returns a Server; Run starts it.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.RepoURL == "":
		return nil, errors.New("catalogserver: repository URL is required")
	case cfg.ReleasesURL == "":
		return nil, errors.New("catalogserver: releases URL is required")
	case cfg.MirrorDir == "":
		return nil, errors.New("catalogserver: mirror directory is required")
	case cfg.SyncInterval <= 0:
		return nil, errors.New("catalogserver: sync interval must be positive")
	case cfg.CacheBytes <= 0:
		return nil, errors.New("catalogserver: cache budget must be positive")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: releaseTime}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		mirror:   &Mirror{Repo: Repo{Dir: cfg.MirrorDir}, URL: cfg.RepoURL},
		releases: releases{base: cfg.ReleasesURL, client: cfg.HTTPClient},
		log:      cfg.Logger,
		cache:    newFileCache(cfg.CacheBytes),
		slots:    make(chan struct{}, maxBuilds),
		now:      time.Now,
		fetchNow: make(chan struct{}, 1),
		syncReqs: syncRequests{gap: extraFetchGap},
		tags:     map[string]*tagState{},
		latest:   map[string]string{},
	}
	if !cfg.DisableWebhooks {
		policy := receiverPolicy{resolver: net.DefaultResolver, allowLoopback: cfg.AllowLoopbackWebhooks}
		version := cfg.Version
		if version == "" {
			version = "dev"
		}
		s.hooks = &webhooks{
			policy:      policy,
			client:      newReceiverClient(policy, nil),
			userAgent:   "wyolet-catalog-server/" + version,
			timeout:     hookTimeout,
			subs:        newSubscriptions(),
			retryDelays: nudgeRetryDelays,
			sendSlots:   make(chan struct{}, nudgeSenders),
			deliverySeq: map[string]uint64{},
		}
	}
	s.metrics = newMetrics(s)
	return s, nil
}

// Run initialises the mirror and syncs it every SyncInterval, and whenever
// an early sync is asked for, until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if err := s.mirror.Init(ctx); err != nil {
		return fmt.Errorf("catalogserver: init mirror %s: %w", s.cfg.MirrorDir, err)
	}
	s.running.Store(true)
	defer s.running.Store(false)
	ticker := time.NewTicker(s.cfg.SyncInterval)
	defer ticker.Stop()
	for {
		answered := s.takeSyncWaiters()
		s.sync(ctx)
		if answered != nil {
			close(answered)
		}
		if s.hooks != nil {
			s.hooks.subs.sweep(s.now())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.fetchNow:
		}
	}
}

// sync keeps everything it already holds when the fetch fails, so an
// unreachable upstream never moves the index or drops a served tag.
func (s *Server) sync(ctx context.Context) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if err := s.mirror.Fetch(ctx); err != nil {
		s.metrics.fetchFailures.Inc()
		s.log.Warn("catalog-server: fetch failed; serving held tags", "err", err)
	} else {
		s.metrics.lastFetch.SetToCurrentTime()
	}
	tags, err := s.mirror.Tags(ctx)
	if err != nil {
		s.log.Error("catalog-server: list tags", "err", err)
		return
	}
	s.addTags(ctx, tags)
	s.fetchSums(ctx)
	s.updateIndex(ctx)
	if ctx.Err() == nil {
		s.mu.Lock()
		s.synced = true
		s.mu.Unlock()
	}
}

func (s *Server) addTags(ctx context.Context, tags []Tag) {
	for _, t := range tags {
		if !isReleaseTag(t.Name) {
			continue
		}
		s.mu.Lock()
		known := s.tags[t.Name]
		if known != nil && known.Commit != t.Commit && !known.moved {
			known.moved = true
			s.log.Warn("catalog-server: tag moved; keeping the commit first seen", "tag", t.Name, "commit", known.Commit, "new_commit", t.Commit)
		}
		s.mu.Unlock()
		if known != nil {
			continue
		}
		st := &tagState{Tag: t}
		err := s.readChannels(ctx, st)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("catalog-server: tag has no single schema channel; never indexed", "tag", t.Name, "err", err)
		}
		s.mu.Lock()
		s.tags[t.Name] = st
		s.mu.Unlock()
	}
}

// readChannels sets st's data channel and, when its tree has mcp/, its MCP
// channel. An mcp/ problem is only logged: it must never keep the tag out of
// /index.yaml.
func (s *Server) readChannels(ctx context.Context, st *tagState) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	mcp, err := s.mirror.MCPArchive(ctx, st.Commit)
	switch {
	case errors.Is(err, ErrNoMCPTree):
	case err != nil:
		s.log.Warn("catalog-server: read mcp tree; its MCP tarball is never served", "tag", st.Name, "err", err)
	default:
		st.hasMCP = true
		if st.mcpChannel, err = MCPArchiveChannel(mcp); err != nil {
			s.log.Warn("catalog-server: mcp tree has no single schema channel; never indexed", "tag", st.Name, "err", err)
		}
	}
	archive, err := s.mirror.Archive(ctx, st.Commit)
	if err != nil {
		return err
	}
	st.channel, err = ArchiveChannel(archive)
	return err
}

// sumDue retries a missing sum every sync while the tag is fresh, hourly after.
func sumDue(committed, checked, now time.Time) bool {
	return checked.IsZero() || now.Sub(committed) < freshTagAge || now.Sub(checked) >= staleSumRetry
}

func (s *Server) fetchSums(ctx context.Context) {
	type job struct {
		st *tagState
		a  asset
	}
	now := s.now()
	var jobs []job
	s.mu.RLock()
	for _, st := range s.tags {
		for _, a := range allAssets {
			if st.expects(a) && st.sums[a].sum == "" && sumDue(st.Committed, st.sums[a].checkedAt, now) {
				jobs = append(jobs, job{st, a})
			}
		}
	}
	s.mu.RUnlock()
	var g errgroup.Group
	g.SetLimit(sumFetchers)
	for _, j := range jobs {
		g.Go(func() error {
			s.fetchSum(ctx, j.st, j.a)
			return nil
		})
	}
	_ = g.Wait()
}

// fetchSum leaves checkedAt unset on a transient error so the next sync
// retries it.
func (s *Server) fetchSum(ctx context.Context, st *tagState, a asset) {
	name := a.fileName(st.Name)
	file, err := s.releases.get(ctx, st.Name, name+".sha256", maxSumBytes)
	sum := ""
	switch {
	case errors.Is(err, errNotPublished):
	case err != nil:
		s.metrics.releaseFailures.WithLabelValues(a.label()).Inc()
		s.log.Debug("catalog-server: fetch published sum", "tag", st.Name, "file", name, "err", err)
		return
	default:
		if sum, err = parseSumFile(file, name); err != nil {
			s.log.Warn("catalog-server: invalid published sum", "tag", st.Name, "file", name+".sha256", "err", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.sums[a].checkedAt = s.now()
	if sum != "" {
		st.sums[a].sum, st.sums[a].file = sum, file
	}
}

// updateIndex makes each channel's latest the highest tag whose files are
// published and verify, building them now, and pins those files. The two
// indexes move independently: an MCP tarball never holds back /index.yaml.
func (s *Server) updateIndex(ctx context.Context) {
	latest := s.latestVerified(ctx, relayFiles)
	mcpLatest := s.latestVerified(ctx, mcpFiles)
	if ctx.Err() != nil {
		return
	}
	relayPins, mcpPins := pinsOf(latest, relayFiles), pinsOf(mcpLatest, mcpFiles)
	s.cache.setPinned(append(slices.Clone(relayPins), mcpPins...))
	if s.holdFiles(ctx, mcpPins) {
		index := renderIndex(mcpLatest)
		s.mu.Lock()
		for channel, tag := range mcpLatest {
			if s.mcpLatest[channel] != tag {
				s.log.Info("catalog-server: mcp channel latest", "channel", channel, "tag", tag)
			}
		}
		s.mcpLatest, s.mcpIndex, s.mcpIndexETag = mcpLatest, index, `"`+sha256Hex(index)+`"`
		s.mu.Unlock()
	}
	if !s.holdFiles(ctx, relayPins) {
		return
	}

	index := renderIndex(latest)
	moved := map[string]string{}
	s.mu.Lock()
	for channel, tag := range latest {
		prev := s.latest[channel]
		if prev != tag {
			s.log.Info("catalog-server: channel latest", "channel", channel, "tag", tag)
		}
		// A channel's first latest since start is no news: relays already
		// had it, and nudging them after every restart would be a herd.
		if prev != "" && semver.Compare(tag, prev) > 0 {
			moved[channel] = tag
		}
	}
	s.latest, s.index, s.indexETag = latest, index, `"`+sha256Hex(index)+`"`
	s.mu.Unlock()
	for channel, tag := range moved {
		s.nudge(ctx, channel, tag)
	}
}

// latestVerified maps each channel of f to its highest tag whose f files are
// published and verify. It stops early, with a partial map, when ctx ends.
func (s *Server) latestVerified(ctx context.Context, f indexedFiles) map[string]string {
	byChannel := map[string][]string{}
	s.mu.RLock()
	for name, st := range s.tags {
		if channel := f.channel(st); channel != "" && st.published(f) {
			byChannel[channel] = append(byChannel[channel], name)
		}
	}
	s.mu.RUnlock()

	latest := map[string]string{}
	for channel, names := range byChannel {
		slices.SortFunc(names, func(a, b string) int { return semver.Compare(b, a) })
		for _, name := range names {
			if ctx.Err() != nil {
				return latest
			}
			if s.verifyTag(ctx, name, f) {
				latest[channel] = name
				break
			}
		}
	}
	return latest
}

func pinsOf(latest map[string]string, f indexedFiles) []fileKey {
	pins := make([]fileKey, 0, len(f.assets)*len(latest))
	for _, tag := range latest {
		for _, a := range f.assets {
			pins = append(pins, fileKey{tag, a})
		}
	}
	return pins
}

// holdFiles loads pinned files evicted since they verified.
func (s *Server) holdFiles(ctx context.Context, pins []fileKey) bool {
	for _, k := range pins {
		if _, err := s.file(ctx, k.tag, k.asset); err != nil {
			return false
		}
	}
	return true
}

func (s *Server) verifyTag(ctx context.Context, tag string, f indexedFiles) bool {
	for _, a := range f.assets {
		if _, err := s.file(ctx, tag, a); err != nil {
			return false
		}
	}
	return true
}

func renderIndex(latest map[string]string) []byte {
	type channel struct {
		Latest string `yaml:"latest"`
	}
	doc := struct {
		Channels map[string]channel `yaml:"channels"`
	}{Channels: make(map[string]channel, len(latest))}
	for ch, tag := range latest {
		doc.Channels[ch] = channel{Latest: tag}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	_ = enc.Encode(doc)
	_ = enc.Close()
	return buf.Bytes()
}

// publishedSum returns tag's sum file for a or, on a miss, whether an early
// sync could still find it: the tag is unknown here, or its sum is due.
func (s *Server) publishedSum(tag string, a asset) (file []byte, catchUp bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.tags[tag]
	switch {
	case st == nil:
		return nil, true
	case st.sums[a].file != nil:
		return st.sums[a].file, false
	case !st.expects(a):
		return nil, false
	}
	return nil, sumDue(st.Committed, st.sums[a].checkedAt, s.now())
}

func (s *Server) ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.synced && len(s.latest) > 0
}
