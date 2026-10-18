package catalogserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
)

const (
	// CI publishes a tag's files a minute or two after the push.
	freshTagAge    = 24 * time.Hour
	staleSumRetry  = time.Hour
	failureRetry   = 5 * time.Minute
	unknownTagTTL  = 60 * time.Second
	extraFetchGap  = 10 * time.Second
	maxUnknownTags = 10_000
	maxBuilds      = 2
	buildTime      = 2 * time.Minute
	sumFetchers    = 4
)

var errUnavailable = errors.New("unavailable")

type asset int

const (
	assetCatalog asset = iota
	assetData
)

var assets = [...]asset{assetCatalog, assetData}

func (a asset) label() string {
	if a == assetData {
		return "data"
	}
	return "catalog"
}

func (a asset) fileName(tag string) string {
	if a == assetData {
		return DataAsset(tag)
	}
	return CatalogAsset
}

func assetNamed(tag, name string) (asset, bool) {
	switch name {
	case CatalogAsset:
		return assetCatalog, true
	case DataAsset(tag):
		return assetData, true
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
	channel  string // "" when the tree has no single apiVersion
	moved    bool
	sums     [2]publishedSum
	verified [2]*verifiedFile
	failedAt [2]time.Time
}

func (t *tagState) published() bool {
	return t.sums[assetCatalog].sum != "" && t.sums[assetData].sum != ""
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

	mu        sync.RWMutex
	tags      map[string]*tagState
	latest    map[string]string // channel → tag
	index     []byte
	indexETag string
	synced    bool

	unknownMu      sync.Mutex
	unknown        map[string]time.Time
	lastExtraFetch time.Time
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
		tags:     map[string]*tagState{},
		latest:   map[string]string{},
		unknown:  map[string]time.Time{},
	}
	s.metrics = newMetrics(s)
	return s, nil
}

// Run initialises the mirror and syncs it every SyncInterval until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if err := s.mirror.Init(ctx); err != nil {
		return fmt.Errorf("catalogserver: init mirror %s: %w", s.cfg.MirrorDir, err)
	}
	ticker := time.NewTicker(s.cfg.SyncInterval)
	defer ticker.Stop()
	for {
		s.sync(ctx)
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
		channel, err := s.channelOf(ctx, t)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("catalog-server: tag has no single schema channel; never indexed", "tag", t.Name, "err", err)
		}
		s.mu.Lock()
		s.tags[t.Name] = &tagState{Tag: t, channel: channel}
		s.mu.Unlock()
	}
}

func (s *Server) channelOf(ctx context.Context, t Tag) (string, error) {
	if err := s.acquire(ctx); err != nil {
		return "", err
	}
	defer s.release()
	archive, err := s.mirror.Archive(ctx, t.Commit)
	if err != nil {
		return "", err
	}
	return ArchiveChannel(archive)
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
		for _, a := range assets {
			if st.sums[a].sum == "" && sumDue(st.Committed, st.sums[a].checkedAt, now) {
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
// published and verify, building them now, and pins those files.
func (s *Server) updateIndex(ctx context.Context) {
	byChannel := map[string][]string{}
	s.mu.RLock()
	for name, st := range s.tags {
		if st.channel != "" && st.published() {
			byChannel[st.channel] = append(byChannel[st.channel], name)
		}
	}
	s.mu.RUnlock()

	latest := map[string]string{}
	for channel, names := range byChannel {
		slices.SortFunc(names, func(a, b string) int { return semver.Compare(b, a) })
		for _, name := range names {
			if ctx.Err() != nil {
				return
			}
			if s.verifyTag(ctx, name) {
				latest[channel] = name
				break
			}
		}
	}
	pins := make([]fileKey, 0, 2*len(latest))
	for _, tag := range latest {
		for _, a := range assets {
			pins = append(pins, fileKey{tag, a})
		}
	}
	s.cache.setPinned(pins)
	for _, k := range pins {
		if _, err := s.file(ctx, k.tag, k.asset); err != nil {
			return
		}
	}

	index := renderIndex(latest)
	s.mu.Lock()
	defer s.mu.Unlock()
	for channel, tag := range latest {
		if s.latest[channel] != tag {
			s.log.Info("catalog-server: channel latest", "channel", channel, "tag", tag)
		}
	}
	s.latest, s.index, s.indexETag = latest, index, `"`+sha256Hex(index)+`"`
}

func (s *Server) verifyTag(ctx context.Context, tag string) bool {
	for _, a := range assets {
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

// file returns the verified bytes of one release file. Concurrent callers
// share one build.
func (s *Server) file(ctx context.Context, tag string, a asset) ([]byte, error) {
	k := fileKey{tag, a}
	if b, ok := s.cache.get(k); ok {
		return b, nil
	}
	ch := s.builds.DoChan(tag+"/"+a.label(), func() (any, error) {
		if b, ok := s.cache.get(k); ok {
			return b, nil
		}
		b, err := s.produce(tag, a)
		if err != nil {
			return nil, err
		}
		s.cache.put(k, b)
		return b, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]byte), nil
	}
}

// produce runs detached from any one request because callers share it.
func (s *Server) produce(tag string, a asset) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTime)
	defer cancel()
	s.mu.RLock()
	st := s.tags[tag]
	if st == nil {
		s.mu.RUnlock()
		return nil, errNotPublished
	}
	t, want, prev, failedAt := st.Tag, st.sums[a].sum, st.verified[a], st.failedAt[a]
	s.mu.RUnlock()
	if want == "" {
		return nil, errNotPublished
	}
	if !failedAt.IsZero() && s.now().Sub(failedAt) < failureRetry {
		return nil, errUnavailable
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()

	name := a.fileName(tag)
	if prev == nil || !prev.fromRelease {
		data, err := s.rebuild(ctx, t, a)
		if err == nil && sha256Hex(data) == want {
			s.setVerified(st, a, &verifiedFile{})
			return data, nil
		}
		if prev == nil {
			if err != nil {
				s.log.Warn("catalog-server: rebuild failed; serving the published file", "tag", tag, "file", name, "err", err)
			} else {
				s.metrics.mismatches.WithLabelValues(a.label()).Inc()
				s.log.Warn("catalog-server: rebuild differs from the published file; serving the published file", "tag", tag, "file", name)
			}
		}
	}
	data, err := s.releases.get(ctx, tag, name, maxAssetBytes)
	if err == nil && sha256Hex(data) != want {
		err = errors.New("does not match its published sha256")
	}
	if err != nil {
		s.metrics.releaseFailures.WithLabelValues(a.label()).Inc()
		s.log.Error("catalog-server: published file unavailable", "tag", tag, "file", name, "err", err)
		s.mu.Lock()
		st.failedAt[a] = s.now()
		s.mu.Unlock()
		return nil, errUnavailable
	}
	s.setVerified(st, a, &verifiedFile{fromRelease: true})
	return data, nil
}

func (s *Server) rebuild(ctx context.Context, t Tag, a asset) ([]byte, error) {
	s.metrics.builds.WithLabelValues(a.label()).Inc()
	data, err := s.mirror.Archive(ctx, t.Commit)
	if err == nil && a == assetCatalog {
		data, err = BuildCatalog(data, t.Committed, t.Name)
	}
	if err != nil {
		s.metrics.buildFailures.WithLabelValues(a.label()).Inc()
	}
	return data, err
}

func (s *Server) setVerified(st *tagState, a asset, v *verifiedFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.verified[a] = v
	st.failedAt[a] = time.Time{}
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() { <-s.slots }

// noteUnknown remembers a missed tag for unknownTagTTL and asks for one
// extra fetch at most every extraFetchGap, so a tag pushed seconds ago is
// found before the next poll.
func (s *Server) noteUnknown(tag string) {
	s.unknownMu.Lock()
	defer s.unknownMu.Unlock()
	now := s.now()
	if exp, ok := s.unknown[tag]; ok && now.Before(exp) {
		return
	}
	if len(s.unknown) >= maxUnknownTags {
		for t, exp := range s.unknown {
			if !now.Before(exp) {
				delete(s.unknown, t)
			}
		}
		if len(s.unknown) >= maxUnknownTags {
			clear(s.unknown)
		}
	}
	s.unknown[tag] = now.Add(unknownTagTTL)
	if now.Sub(s.lastExtraFetch) < extraFetchGap {
		return
	}
	s.lastExtraFetch = now
	select {
	case s.fetchNow <- struct{}{}:
	default:
	}
}

func (s *Server) ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.synced && len(s.latest) > 0
}
