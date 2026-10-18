package catalogserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureAPIVersion = "relay.wyolet.dev/v1alpha2"

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// catalogTree is a small valid catalog; displayName varies between commits.
func catalogTree(apiVersion, displayName string) map[string]string {
	return map[string]string{
		"data/providers/acme/provider.yaml": `apiVersion: ` + apiVersion + `
kind: Provider
metadata:
  name: acme
  displayName: Acme
spec:
  enabled: true
`,
		"data/providers/acme/models/rocket.yaml": `apiVersion: ` + apiVersion + `
kind: Model
metadata:
  name: rocket
  displayName: ` + displayName + `
  owner:
    kind: provider
    id: acme
spec:
  enabled: true
  pointer: rocket-1
  snapshots:
    - name: rocket-1
`,
		"data/hosts/acme-api/host.yaml": `apiVersion: ` + apiVersion + `
kind: Host
metadata:
  name: acme-api
  displayName: Acme API
spec:
  baseURL: https://api.example.com
  enabled: true
---
apiVersion: ` + apiVersion + `
kind: HostBinding
metadata:
  name: rocket-acme-api
  owner:
    kind: system
spec:
  model: rocket
  host: acme-api
  adapter: openai
`,
		"data/drafts/hosts/wip/host.yaml": "apiVersion: relay.wyolet.dev/v0draft\nkind: Host\n",
		"README.md":                       "not part of any release\n",
	}
}

// sourceRepo is the upstream catalog repository a mirror fetches from.
type sourceRepo struct {
	t   *testing.T
	dir string
}

func newSourceRepo(t *testing.T) *sourceRepo {
	t.Helper()
	requireGit(t)
	r := &sourceRepo{t: t, dir: filepath.Join(t.TempDir(), "relay-catalog")}
	r.git(time.Time{}, "init", "--quiet", "--initial-branch=main", r.dir)
	return r
}

func (r *sourceRepo) git(at time.Time, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	if len(args) == 0 || args[0] != "init" {
		cmd.Dir = r.dir
	}
	cmd.Env = append(gitEnv(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if !at.IsZero() {
		date := at.Format(time.RFC3339)
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit replaces the work tree with files and commits it at at.
func (r *sourceRepo) commit(files map[string]string, at time.Time) string {
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
	r.git(at, "add", "-A")
	r.git(at, "commit", "--quiet", "--allow-empty", "-m", "data")
	return r.git(at, "rev-parse", "HEAD")
}

func (r *sourceRepo) tag(name string, annotated bool) {
	r.t.Helper()
	if annotated {
		r.git(time.Now(), "tag", "-a", "-m", name, name)
		return
	}
	r.git(time.Time{}, "tag", name)
}

// releaseFiles builds a tag's two files the way the release CI does.
func (r *sourceRepo) releaseFiles(tag string) (data, catalog []byte) {
	r.t.Helper()
	repo := Repo{Dir: r.dir}
	commit, at, err := repo.Commit(context.Background(), tag)
	if err != nil {
		r.t.Fatal(err)
	}
	if data, err = BuildData(context.Background(), repo, commit); err != nil {
		r.t.Fatal(err)
	}
	if catalog, err = BuildCatalog(data, at, tag); err != nil {
		r.t.Fatal(err)
	}
	return data, catalog
}

// fakeReleases stands in for GitHub release downloads. Files redirect to a
// second path, the way GitHub hands downloads to its object store.
type fakeReleases struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	srv   *httptest.Server
}

func newFakeReleases(t *testing.T) *fakeReleases {
	f := &fakeReleases{files: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/objects/"); ok {
			f.mu.Lock()
			b, ok := f.files[rest]
			f.hits[rest]++
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		_, ok := f.files[key]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/objects/"+key, http.StatusFound)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// publish uploads name and a sum file of sumOf (normally data itself).
func (f *fakeReleases) publish(tag, name string, data, sumOf []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[tag+"/"+name] = data
	f.files[tag+"/"+name+".sha256"] = SumFile(name, sumOf)
}

func (f *fakeReleases) publishBoth(tag string, data, catalog []byte) {
	f.publish(tag, DataAsset(tag), data, data)
	f.publish(tag, CatalogAsset, catalog, catalog)
}

func (f *fakeReleases) fileHits(tag, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[tag+"/"+name]
}

func newTestServer(t *testing.T, src *sourceRepo, rel *fakeReleases, cacheBytes int64) *Server {
	t.Helper()
	s, err := New(Config{
		RepoURL:      src.dir,
		ReleasesURL:  rel.srv.URL,
		MirrorDir:    filepath.Join(t.TempDir(), "mirror"),
		SyncInterval: time.Hour,
		CacheBytes:   cacheBytes,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.mirror.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, s *Server, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func indexLatest(t *testing.T, s *Server) map[string]string {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.latest))
	for k, v := range s.latest {
		out[k] = v
	}
	return out
}

var (
	oldCommit = time.Date(2026, 7, 1, 20, 7, 4, 0, time.FixedZone("", 5*3600))
	newCommit = time.Date(2026, 8, 1, 9, 0, 0, 0, time.FixedZone("", -4*3600))
)
