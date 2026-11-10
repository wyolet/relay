//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/catalogserver"
)

// The catalog-follow suite runs a real catalog server over a scratch git
// repository and boots the relay binary against it, the way a deployment
// wires the two. Catalog data is a small v1alpha3 tree; each tag may add
// models on top of it.

const followChannel = "v1alpha3"

// catalogTreeV3 is a minimal valid v1alpha3 catalog serving the named models
// from one host.
func catalogTreeV3(models ...string) map[string]string {
	files := map[string]string{
		"data/providers/anthropic/provider.yaml": `apiVersion: relay.wyolet.dev/v1alpha3
kind: Provider
metadata:
    name: anthropic
    displayName: Anthropic
spec:
    homepageURL: https://www.anthropic.com
`,
		"data/hosts/anthropic/host.yaml": `apiVersion: relay.wyolet.dev/v1alpha3
kind: Host
metadata:
    name: anthropic
    displayName: Anthropic
spec:
    baseURL: https://api.anthropic.com
`,
		"README.md": "not part of any release\n",
	}
	for _, m := range models {
		files["data/providers/anthropic/models/"+m+".yaml"] = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Model
metadata:
    name: ` + m + `
    displayName: ` + m + `
    owner:
        kind: provider
        name: anthropic
spec:
    capabilities:
        chat: true
        streaming: true
    modalities:
        input: [text]
        output: [text]
    contextWindowInput: 200000
    contextWindowOutput: 64000
    snapshots:
        - name: ` + m + `
    pointer: ` + m + `
`
		files["data/hosts/anthropic/bindings/"+m+".yaml"] = `apiVersion: relay.wyolet.dev/v1alpha3
kind: HostBinding
metadata:
    name: ` + m + `-on-anthropic
spec:
    model: ` + m + `
    host: anthropic
    adapter: anthropic
`
	}
	return files
}

// catalogRepo is the upstream catalog repository the server mirrors.
type catalogRepo struct {
	t   *testing.T
	dir string
}

func newCatalogRepo(t *testing.T) *catalogRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := &catalogRepo{t: t, dir: filepath.Join(t.TempDir(), "relay-catalog")}
	r.git("init", "--quiet", "--initial-branch=main", r.dir)
	return r
}

func (r *catalogRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	if args[0] != "init" {
		cmd.Dir = r.dir
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitTag replaces the tree with files, commits and tags it.
func (r *catalogRepo) commitTag(tag string, files map[string]string) {
	r.t.Helper()
	for _, sub := range []string{"data", "README.md"} {
		if err := os.RemoveAll(filepath.Join(r.dir, sub)); err != nil {
			r.t.Fatal(err)
		}
	}
	for name, body := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "--quiet", "--allow-empty", "-m", "data "+tag)
	r.git("tag", "-a", "-m", tag, tag)
}

// releaseFiles builds a tag's data tarball and catalog.json.gz with the
// build code `catalog-server build` runs.
func (r *catalogRepo) releaseFiles(tag string) (data, catalog []byte) {
	r.t.Helper()
	ctx := context.Background()
	repo := catalogserver.Repo{Dir: r.dir}
	commit, at, err := repo.Commit(ctx, tag)
	if err != nil {
		r.t.Fatal(err)
	}
	if data, err = catalogserver.BuildData(ctx, repo, commit); err != nil {
		r.t.Fatal(err)
	}
	if catalog, err = catalogserver.BuildCatalog(data, at, tag); err != nil {
		r.t.Fatal(err)
	}
	return data, catalog
}

// releaseHost stands in for GitHub release downloads: <base>/<tag>/<file>,
// redirected to an object path the way GitHub hands a download to storage.
type releaseHost struct {
	mu    sync.Mutex
	files map[string][]byte
	srv   *httptest.Server
}

func newReleaseHost(t *testing.T) *releaseHost {
	h := &releaseHost{files: map[string][]byte{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/objects/"); ok {
			h.mu.Lock()
			b, ok := h.files[rest]
			h.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		h.mu.Lock()
		_, ok := h.files[key]
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/objects/"+key, http.StatusFound)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *releaseHost) publish(tag string, data, catalog []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, b := range map[string][]byte{catalogserver.DataAsset(tag): data, catalogserver.CatalogAsset: catalog} {
		h.files[tag+"/"+name] = b
		h.files[tag+"/"+name+".sha256"] = catalogserver.SumFile(name, b)
	}
}

// catalogWorld is a catalog repository, its release host and a running
// catalog server mirroring them.
type catalogWorld struct {
	t        *testing.T
	repo     *catalogRepo
	releases *releaseHost
	srv      *catalogserver.Server
	http     *httptest.Server
	logs     *lockedBuffer
	clients  int // registrations sent, for distinct client addresses
}

type worldOptions struct {
	syncInterval  time.Duration // 0: one hour, so only /hooks/github syncs early
	allowLoopback bool
	disableHooks  bool
}

// newCatalogWorld publishes tag (built from models) and starts a server that
// has synced it.
func newCatalogWorld(t *testing.T, opts worldOptions, tag string, models ...string) *catalogWorld {
	t.Helper()
	w := &catalogWorld{t: t, repo: newCatalogRepo(t), releases: newReleaseHost(t), logs: &lockedBuffer{}}
	w.publishTag(tag, models...)
	if opts.syncInterval == 0 {
		opts.syncInterval = time.Hour
	}
	srv, err := catalogserver.New(catalogserver.Config{
		RepoURL:               w.repo.dir,
		ReleasesURL:           w.releases.srv.URL,
		MirrorDir:             filepath.Join(t.TempDir(), "mirror"),
		SyncInterval:          opts.syncInterval,
		CacheBytes:            16 << 20,
		Logger:                slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DisableWebhooks:       opts.disableHooks,
		AllowLoopbackWebhooks: opts.allowLoopback,
		// The test process is the "proxy": each registration names its own
		// client in X-Forwarded-For, so the per-client limit is testable.
		TrustedProxies: []*net.IPNet{{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(32, 32)}},
		Version:        "contract-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	w.srv = srv
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Run(ctx) }()
	w.http = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		w.http.Close()
		cancel()
		<-done
		if t.Failed() {
			t.Logf("catalog server logs:\n%s", tail(w.logs.String(), 40))
		}
	})
	waitFor(t, 30*time.Second, "catalog server ready", func() bool {
		res, err := http.Get(w.http.URL + "/readyz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK && w.indexLatest() == tag
	})
	return w
}

// publishTag tags the tree of models and uploads its release files, the order
// release CI runs in.
func (w *catalogWorld) publishTag(tag string, models ...string) {
	w.t.Helper()
	w.repo.commitTag(tag, catalogTreeV3(models...))
	data, cat := w.repo.releaseFiles(tag)
	w.releases.publish(tag, data, cat)
}

// pushHook calls the server's repository push hook, as GitHub would.
func (w *catalogWorld) pushHook() {
	w.t.Helper()
	res, err := http.Post(w.http.URL+"/hooks/github", "application/json", strings.NewReader(`{"ref":"refs/tags/x"}`))
	if err != nil {
		w.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		w.t.Fatalf("POST /hooks/github = %d, want 202", res.StatusCode)
	}
}

// indexLatest is the channel's latest tag in the served index, "" if none.
func (w *catalogWorld) indexLatest() string {
	res, err := http.Get(w.http.URL + "/index.yaml")
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return indexLatestOf(string(body), followChannel)
}

// indexLatestOf reads channels.<channel>.latest from an index body without
// assuming more of its layout than that.
func indexLatestOf(body, channel string) string {
	in := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, channel+":") {
			in = true
			continue
		}
		if in {
			if v, ok := strings.CutPrefix(line, "latest:"); ok {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
			if strings.HasSuffix(line, ":") {
				in = false
			}
		}
	}
	return ""
}

func (w *catalogWorld) metric(name string, labels ...string) float64 {
	return scrapeMetric(w.t, w.http.URL+"/metrics", "", name, labels...)
}

// register posts a webhook registration as a client address of its own.
func (w *catalogWorld) register(url, channel string) (int, string) {
	w.t.Helper()
	w.clients++
	return w.registerFrom(fmt.Sprintf("198.51.%d.%d", w.clients/250, w.clients%250+1), url, channel)
}

// registerFrom posts a webhook registration on behalf of client.
func (w *catalogWorld) registerFrom(client, url, channel string) (int, string) {
	w.t.Helper()
	body, _ := json.Marshal(map[string]string{"url": url, "channel": channel})
	req, _ := http.NewRequest(http.MethodPost, w.http.URL+"/v1/webhooks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", client)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// hookSpy is a release-webhook receiver that records what the server sends.
type hookSpy struct {
	srv *httptest.Server

	mu           sync.Mutex
	challengeMod func(string) (int, string) // nil: echo
	releaseCode  int                        // 0: 202
	releaseDelay time.Duration              // before answering a release
	releases     []spyRequest
	challenges   []spyRequest
}

type spyRequest struct {
	at          time.Time
	body        map[string]any
	userAgent   string
	contentType string
}

func newHookSpy(t *testing.T) *hookSpy {
	s := &hookSpy{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		req := spyRequest{at: time.Now(), body: msg, userAgent: r.UserAgent(), contentType: r.Header.Get("Content-Type")}
		s.mu.Lock()
		switch msg["type"] {
		case "challenge":
			s.challenges = append(s.challenges, req)
			mod := s.challengeMod
			s.mu.Unlock()
			if mod != nil {
				code, body := mod(fmt.Sprint(msg["challenge"]))
				w.WriteHeader(code)
				_, _ = io.WriteString(w, body)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": msg["challenge"]})
		case "release":
			s.releases = append(s.releases, req)
			code, delay := s.releaseCode, s.releaseDelay
			s.mu.Unlock()
			if delay > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
			}
			if code == 0 {
				code = http.StatusAccepted
			}
			w.WriteHeader(code)
		default:
			s.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *hookSpy) url() string { return s.srv.URL + "/catalog/hooks/release" }

func (s *hookSpy) releaseRequests() []spyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spyRequest(nil), s.releases...)
}

func (s *hookSpy) challengeRequests() []spyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spyRequest(nil), s.challenges...)
}
