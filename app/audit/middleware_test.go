package audit

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
)

// memSink collects written events for assertions.
type memSink struct {
	mu     sync.Mutex
	events []Event
	before time.Time
	pruned int64
}

func (m *memSink) Write(_ context.Context, evs []Event) error {
	m.mu.Lock()
	m.events = append(m.events, evs...)
	m.mu.Unlock()
	return nil
}

func (m *memSink) Prune(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	m.before = before
	m.pruned++
	m.mu.Unlock()
	return 1, nil
}

func (m *memSink) all() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serve runs one request through the audit middleware with a handler that
// authorizes once and writes code, and returns the events that reached the
// sink.
func serve(t *testing.T, a *actor.Actor, method, target string, remoteAddr, xff string, action string, code int) []Event {
	t.Helper()
	sink := &memSink{}
	em := NewEmitter(sink, quietLogger())
	authzr := Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}

	h := Middleware(em, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = authzr.Authorize(r.Context(), action, authz.Resource{Kind: "policy"})
		w.WriteHeader(code)
	}))

	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if a != nil {
		req = req.WithContext(actor.WithActor(req.Context(), a))
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	em.Close()
	return sink.all()
}

func TestMiddlewareActorKinds(t *testing.T) {
	tests := []struct {
		name     string
		actor    *actor.Actor
		wantKind string
		wantID   string
		wantName string
		wantSess string
	}{
		{
			name:     "session user",
			actor:    &actor.Actor{UserID: "u-1", Username: "alice", SessionID: "s-1"},
			wantKind: ActorUser, wantID: "u-1", wantName: "alice", wantSess: "s-1",
		},
		{
			name:     "admin token",
			actor:    &actor.Actor{AdminToken: true, Username: "admin-token"},
			wantKind: ActorAdminToken, wantName: "admin-token",
		},
		{
			name:     "no actor",
			actor:    nil,
			wantKind: ActorAnonymous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs := serve(t, tt.actor, http.MethodPut, "/api/policies/by-id/p-1", "10.0.0.7:5555", "", "policies.update", http.StatusOK)
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			got := evs[0].Actor
			if got.Kind != tt.wantKind || got.ID != tt.wantID || got.Name != tt.wantName || got.SessionID != tt.wantSess {
				t.Fatalf("actor = %+v, want kind=%q id=%q name=%q session=%q", got, tt.wantKind, tt.wantID, tt.wantName, tt.wantSess)
			}
			if got.IP != "10.0.0.7" {
				t.Fatalf("actor.IP = %q, want the peer address 10.0.0.7", got.IP)
			}
		})
	}
}

// With no trusted proxies configured, a caller-supplied forwarding header
// must never become the audited address.
func TestMiddlewareIgnoresForwardedFor(t *testing.T) {
	evs := serve(t, &actor.Actor{UserID: "u-1"}, http.MethodPost, "/api/policies", "10.0.0.7:5555", "203.0.113.5", "policies.create", http.StatusCreated)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].Actor.IP; got != "10.0.0.7" {
		t.Fatalf("actor.IP = %q, want 10.0.0.7 (peer), not the X-Forwarded-For hop", got)
	}
}

func TestMiddlewareFillsRequestAndCode(t *testing.T) {
	evs := serve(t, &actor.Actor{UserID: "u-1"}, http.MethodDelete, "/api/policies/by-id/p-1", "10.0.0.7:5555", "", "policies.delete", http.StatusNoContent)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Request.Method != http.MethodDelete || ev.Request.Path != "/api/policies/by-id/p-1" {
		t.Fatalf("request = %+v", ev.Request)
	}
	if ev.Outcome.Code != http.StatusNoContent || ev.Outcome.Status != StatusAllowed {
		t.Fatalf("outcome = %+v, want 204/allowed", ev.Outcome)
	}
	if ev.ID == "" || ev.TS.IsZero() {
		t.Fatalf("id/ts not stamped: %+v", ev)
	}
}

func TestMiddlewareSkipsAllowedRead(t *testing.T) {
	evs := serve(t, &actor.Actor{UserID: "u-1"}, http.MethodGet, "/api/policies", "10.0.0.7:5555", "", "policies.list", http.StatusOK)
	if len(evs) != 0 {
		t.Fatalf("events = %d, want 0 — an allowed read earns no row", len(evs))
	}
}

// Behind a trusted proxy the audited address is the forwarded client, not
// the gateway — the k8s ingress case.
func TestMiddlewareHonoursForwardedForBehindATrustedProxy(t *testing.T) {
	sink := &memSink{}
	em := NewEmitter(sink, quietLogger())
	_, trusted, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	authzr := Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}
	h := Middleware(em, []*net.IPNet{trusted})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = authzr.Authorize(r.Context(), "policies.update", authz.Resource{Kind: "policy"})
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPut, "/api/policies/by-id/p-1", nil)
	req.RemoteAddr = "10.0.0.7:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.7")
	req = req.WithContext(actor.WithActor(req.Context(), &actor.Actor{UserID: "u-1"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].Actor.IP; got != "203.0.113.5" {
		t.Fatalf("actor.IP = %q, want the forwarded client 203.0.113.5", got)
	}
}

func TestChangedAndRecordAreNoOpsOutsideARequest(t *testing.T) {
	Changed(context.Background(), []string{"spec.enabled"})
	Record(context.Background(), "auth.login", Resource{Kind: "user"}, StatusAllowed)
}
