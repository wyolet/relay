package cataloghook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// catalogServer records registrations and answers each with the status the
// test sets.
type catalogServer struct {
	mu     sync.Mutex
	bodies []map[string]string
	status int
	srv    *httptest.Server
}

func newCatalogServer(t *testing.T) *catalogServer {
	t.Helper()
	cs := &catalogServer{status: http.StatusAccepted}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/webhooks" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, body)
		status := cs.status
		cs.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"expiresAt":"2026-11-11T12:00:00Z"}`))
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *catalogServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.bodies)
}

func (cs *catalogServer) setStatus(s int) {
	cs.mu.Lock()
	cs.status = s
	cs.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewRegistrarTargetsTheIndexOrigin(t *testing.T) {
	r, err := NewRegistrar("https://catalog.example.com/index.yaml", "https://relay.example.com/", "v1alpha3")
	if err != nil {
		t.Fatal(err)
	}
	if r.Endpoint != "https://catalog.example.com/v1/webhooks" {
		t.Errorf("Endpoint = %q", r.Endpoint)
	}
	if r.HookURL != "https://relay.example.com/catalog/hooks/release" {
		t.Errorf("HookURL = %q", r.HookURL)
	}
	if r.Renew != 6*time.Hour || r.Retry != time.Minute {
		t.Errorf("Renew = %v, Retry = %v", r.Renew, r.Retry)
	}
	def, err := NewRegistrar("", "https://relay.example.com", "v1alpha3")
	if err != nil || def.Endpoint != "https://catalog.wyolet.com/v1/webhooks" {
		t.Errorf("default Endpoint = %q, %v", def.Endpoint, err)
	}
	if _, err := NewRegistrar("index.yaml", "https://relay.example.com", "v1alpha3"); err == nil {
		t.Error("an index URL with no origin must be refused")
	}
}

// Run registers at once with the contract's body, then again on every renewal.
func TestRegistrarRegistersAndRenews(t *testing.T) {
	cs := newCatalogServer(t)
	r, err := NewRegistrar(cs.srv.URL+"/index.yaml", "https://relay.example.com", "v1alpha3")
	if err != nil {
		t.Fatal(err)
	}
	r.Renew, r.Retry = 30*time.Millisecond, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, "three registrations", func() bool { return cs.count() >= 3 })
	cancel()
	<-done

	cs.mu.Lock()
	first := cs.bodies[0]
	cs.mu.Unlock()
	if first["url"] != "https://relay.example.com/catalog/hooks/release" || first["channel"] != "v1alpha3" {
		t.Fatalf("registration body = %v", first)
	}
}

// A refused registration is retried once after Retry, not in a loop; the
// renewal still comes.
func TestRegistrarRetriesAFailureOnce(t *testing.T) {
	cs := newCatalogServer(t)
	cs.setStatus(http.StatusForbidden)
	r, err := NewRegistrar(cs.srv.URL, "https://relay.example.com", "v1alpha3")
	if err != nil {
		t.Fatal(err)
	}
	r.Renew, r.Retry = 400*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the retry", func() bool { return cs.count() >= 2 })
	time.Sleep(150 * time.Millisecond)
	if n := cs.count(); n != 2 {
		t.Fatalf("attempts before the renewal = %d, want 2 (first + one retry)", n)
	}
	cs.setStatus(http.StatusAccepted)
	waitFor(t, "the renewal", func() bool { return cs.count() >= 3 })
}
