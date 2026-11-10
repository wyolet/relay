package catalogserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"
)

const testChannel = "v1alpha2"

// fakeResolver answers each lookup of a host with the next of its answers;
// the last one repeats.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
}

func (f *fakeResolver) set(host string, answers ...[]netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = answers
}

func (f *fakeResolver) lookups(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq, ok := f.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := min(f.calls[host], len(seq)-1)
	f.calls[host]++
	return seq[i], nil
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// receiver stands in for a relay's release hook.
type receiver struct {
	srv *httptest.Server

	mu           sync.Mutex
	challenge    string // "echo" (default), "wrong", "error" or "slow"
	failReleases int    // releases to answer 503 before 202; -1 for all
	challenges   []string
	releases     []map[string]string
	hosts        []string
	agents       []string
	serverNames  []string
	onRelease    func(tag string)
}

func newReceiver(t *testing.T, useTLS bool) *receiver {
	rc := &receiver{challenge: "echo"}
	if useTLS {
		rc.srv = httptest.NewTLSServer(rc)
	} else {
		rc.srv = httptest.NewServer(rc)
	}
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var msg map[string]string
	_ = json.NewDecoder(r.Body).Decode(&msg)
	rc.mu.Lock()
	rc.hosts = append(rc.hosts, r.Host)
	rc.agents = append(rc.agents, r.UserAgent())
	if r.TLS != nil {
		rc.serverNames = append(rc.serverNames, r.TLS.ServerName)
	}
	switch msg["type"] {
	case "challenge":
		rc.challenges = append(rc.challenges, msg["challenge"])
		mode := rc.challenge
		rc.mu.Unlock()
		switch mode {
		case "wrong":
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": "0123"})
		case "error":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "slow":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": msg["challenge"]})
		}
	case "release":
		rc.releases = append(rc.releases, msg)
		fail := rc.failReleases != 0
		if rc.failReleases > 0 {
			rc.failReleases--
		}
		hook := rc.onRelease
		rc.mu.Unlock()
		if hook != nil {
			hook(msg["tag"])
		}
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		rc.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (rc *receiver) port() string {
	u, _ := url.Parse(rc.srv.URL)
	return u.Port()
}

func (rc *receiver) releaseCount() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.releases)
}

func (rc *receiver) challengeCount() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.challenges)
}

// newHookServer is a server whose index knows testChannel, with no mirror:
// webhook tests need no git. Receivers resolve through the returned resolver.
func newHookServer(t *testing.T, allowLoopback bool) (*Server, *fakeResolver) {
	t.Helper()
	s, err := New(Config{
		RepoURL:      "https://example.invalid/catalog.git",
		ReleasesURL:  "https://example.invalid/releases",
		MirrorDir:    t.TempDir(),
		SyncInterval: time.Hour,
		CacheBytes:   1 << 20,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:      "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.latest = map[string]string{testChannel: "v0.1.0"}
	res := newFakeResolver()
	useReceiverPolicy(s, receiverPolicy{resolver: res, allowLoopback: allowLoopback}, nil)
	s.hooks.timeout = 500 * time.Millisecond
	s.hooks.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return s, res
}

func useReceiverPolicy(s *Server, p receiverPolicy, tlsConfig *tls.Config) {
	s.hooks.policy = p
	s.hooks.client = newReceiverClient(p, tlsConfig)
}

func trustReceiver(s *Server, rc *receiver) {
	pool := x509.NewCertPool()
	pool.AddCert(rc.srv.Certificate())
	useReceiverPolicy(s, s.hooks.policy, &tls.Config{RootCAs: pool})
}

func register(t *testing.T, s *Server, hookURL, channel, remote string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"url": hookURL, "channel": channel})
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", bytes.NewReader(body))
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func subCount(s *Server) int {
	s.hooks.subs.mu.Lock()
	defer s.hooks.subs.mu.Unlock()
	return len(s.hooks.subs.subs)
}

const clientAddr = "198.51.100.7:40000"
