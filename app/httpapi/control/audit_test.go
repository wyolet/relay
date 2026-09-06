package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/kv"
)

// auditSink collects the events an audited request produced.
type auditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *auditSink) Write(_ context.Context, evs []audit.Event) error {
	s.mu.Lock()
	s.events = append(s.events, evs...)
	s.mu.Unlock()
	return nil
}

func (s *auditSink) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *auditSink) all() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.Event(nil), s.events...)
}

// newAuditHarness mounts one CRUD kind behind the audit middleware and the
// audit Authorizer wrapper, with the actor named by X-Test-Actor injected
// the way the session/admin-token middlewares would.
func newAuditHarness(t *testing.T, inner authz.Authorizer, seed ...*scopedThing) (http.Handler, *auditSink, *audit.Emitter) {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tmeta := func(v *scopedThing) *meta.Metadata { return &v.Meta }
	store := &memStore[scopedThing]{metaOf: tmeta, items: map[string]*scopedThing{}}
	for _, it := range seed {
		store.items[it.Meta.ID] = it
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Use(audit.Middleware(em, nil))
	api := humachi.New(r, huma.DefaultConfig("audit-test", "0"))
	registerKind[scopedThing](
		api, "rate-limits", "rate-limit", store,
		audit.Authorizer{Inner: inner}, tmeta,
		nil,
		meta.OwnerUser,
		listScanResolver[scopedThing](store, tmeta),
		nil, nil, nil, nil, nil,
		noSettings{},
		false,
		nil,
		nil,
	)
	return r, sink, em
}

func TestAuditDeniedMutationByNonOwner(t *testing.T) {
	h, sink, em := newAuditHarness(t, testRBAC(), seedThings()...)

	// A catalog-owned row is visible to everyone but mutable by admins
	// only, so the Authorizer itself denies and the status is 403.
	w := scopeReq(t, h, "bob", http.MethodDelete, "/rate-limits/by-id/"+catalogID, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "rate-limits.delete" || ev.Outcome.Status != audit.StatusDenied || ev.Outcome.Code != http.StatusForbidden {
		t.Fatalf("event = action %q outcome %+v, want rate-limits.delete denied/403", ev.Action, ev.Outcome)
	}
	if ev.Actor.Kind != audit.ActorUser || ev.Actor.ID != "u-bob" {
		t.Fatalf("actor = %+v, want the denied caller", ev.Actor)
	}
	if ev.Resource.Kind != "rate-limit" || ev.Resource.ID != catalogID {
		t.Fatalf("resource = %+v, want the targeted row", ev.Resource)
	}
}

// A row owned by another user is invisible, so the handler 404s before any
// Authorize call — the middleware's route fallback is what records it.
func TestAuditMutationRefusedByVisibility(t *testing.T) {
	h, sink, em := newAuditHarness(t, testRBAC(), seedThings()...)

	w := scopeReq(t, h, "bob", http.MethodDelete, "/rate-limits/by-id/"+aliceID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "rate-limits.delete" || ev.Outcome.Status != audit.StatusDenied || ev.Outcome.Code != http.StatusNotFound {
		t.Fatalf("event = action %q outcome %+v, want rate-limits.delete denied/404", ev.Action, ev.Outcome)
	}
	if ev.Resource.ID != aliceID {
		t.Fatalf("resource = %+v, want the targeted row id", ev.Resource)
	}
	if ev.Actor.ID != "u-bob" {
		t.Fatalf("actor = %+v, want the refused caller", ev.Actor)
	}
}

func TestAuditAllowedUpdateRecordsChangedPaths(t *testing.T) {
	h, sink, em := newAuditHarness(t, testRBAC(), seedThings()...)

	body := `{"metadata":{"id":"` + aliceID + `","name":"alice-row","displayName":"Renamed"}}`
	w := scopeReq(t, h, "alice", http.MethodPut, "/rate-limits/by-id/"+aliceID, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Outcome.Status != audit.StatusAllowed || ev.Action != "rate-limits.update" {
		t.Fatalf("event = action %q outcome %+v", ev.Action, ev.Outcome)
	}
	if ev.Change == nil || !contains(ev.Change.Fields, "metadata.displayName") {
		t.Fatalf("change = %+v, want metadata.displayName", ev.Change)
	}
}

func TestAuditSkipsAllowedRead(t *testing.T) {
	h, sink, em := newAuditHarness(t, testRBAC(), seedThings()...)
	if w := scopeReq(t, h, "alice", http.MethodGet, "/rate-limits", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	em.Close()
	if evs := sink.all(); len(evs) != 0 {
		t.Fatalf("events = %d, want 0 for an allowed list", len(evs))
	}
}

// newAuthAuditHarness mounts /auth/* behind the session and audit
// middlewares with a single YAML-backed user.
func newAuthAuditHarness(t *testing.T) (http.Handler, *auditSink, *audit.Emitter) {
	t.Helper()
	dir := t.TempDir()
	yaml := "apiVersion: relay.wyolet.dev/v1\nkind: User\nmetadata:\n  name: u-alice\nspec:\n  username: alice\n  email: alice@example.com\n  password: secret123\n  roles: [admin]\n"
	if err := os.WriteFile(filepath.Join(dir, "alice.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write user yaml: %v", err)
	}
	idStore, err := identity.LoadYAML(dir)
	if err != nil {
		t.Fatalf("identity.LoadYAML: %v", err)
	}

	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })
	sessions := session.New(kvStore, false, "sess:")

	r := chi.NewRouter()
	r.Use(sessions.Middleware)
	r.Use(audit.Middleware(em, nil))
	api := humachi.New(r, huma.DefaultConfig("auth-audit-test", "0"))
	registerAuth(api, Deps{Identity: idStore, Sessions: sessions})
	return r, sink, em
}

func TestAuditLoginSuccess(t *testing.T) {
	h, sink, em := newAuthAuditHarness(t)
	if w := authPost(t, h, "/auth/login", `{"username":"alice","password":"secret123"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusAllowed || ev.Outcome.Code != http.StatusOK {
		t.Fatalf("event = action %q outcome %+v, want auth.login allowed/200", ev.Action, ev.Outcome)
	}
	if ev.Actor.Kind != audit.ActorUser || ev.Actor.Name != "alice" {
		t.Fatalf("actor = %+v, want the logged-in user", ev.Actor)
	}
	if ev.Resource.Kind != "user" || ev.Resource.Name != "alice" {
		t.Fatalf("resource = %+v, want the user row", ev.Resource)
	}
}

func TestAuditLoginFailure(t *testing.T) {
	h, sink, em := newAuthAuditHarness(t)
	if w := authPost(t, h, "/auth/login", `{"username":"alice","password":"wrong"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "auth.login" || ev.Outcome.Status != audit.StatusDenied || ev.Outcome.Code != http.StatusUnauthorized {
		t.Fatalf("event = action %q outcome %+v, want auth.login denied/401", ev.Action, ev.Outcome)
	}
	if ev.Actor.Kind != audit.ActorAnonymous || ev.Actor.Name != "alice" {
		t.Fatalf("actor = %+v, want anonymous carrying the attempted username", ev.Actor)
	}
	if ev.Actor.ID != "" {
		t.Fatalf("actor.ID = %q, want empty for a failed login", ev.Actor.ID)
	}
}

func TestAuditLogout(t *testing.T) {
	h, sink, em := newAuthAuditHarness(t)
	if w := authPost(t, h, "/auth/logout", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
	}
	em.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "auth.logout" || ev.Outcome.Status != audit.StatusAllowed {
		t.Fatalf("event = action %q outcome %+v, want auth.logout allowed", ev.Action, ev.Outcome)
	}
	if ev.Actor.Kind != audit.ActorAnonymous {
		t.Fatalf("actor = %+v, want anonymous for a logout with no live session", ev.Actor)
	}
}

// audit.read is granted only by a binding; with no bindings in the snapshot
// it stays admin-only, the same rule that gates settings and debug.
func TestAuditReadIsAdminOnlyUnderRBAC(t *testing.T) {
	for _, who := range []string{"alice", "bob"} {
		ctx := actor.WithActor(t.Context(), scopeActors[who])
		if err := testRBAC().Authorize(ctx, "audit.read", authz.Resource{Kind: "audit"}); err == nil {
			t.Fatalf("%s: audit.read allowed, want denied", who)
		}
	}
	for _, who := range []string{"root", "token"} {
		ctx := actor.WithActor(t.Context(), scopeActors[who])
		if err := testRBAC().Authorize(ctx, "audit.read", authz.Resource{Kind: "audit"}); err != nil {
			t.Fatalf("%s: audit.read denied (%v), want allowed", who, err)
		}
	}
}

// execOnlyDBTX satisfies gen.DBTX with just enough to back UpsertSetting
// (an Exec) — every other method is unused on this path.
type execOnlyDBTX struct{}

func (execOnlyDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (execOnlyDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("execOnlyDBTX: Query not implemented")
}
func (execOnlyDBTX) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (execOnlyDBTX) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("execOnlyDBTX: CopyFrom not implemented")
}

// A bare "value" path is a secret-bearing leaf and gets filtered to an empty
// change set; the settings PUT handler must record the section path instead
// so the update leaves a change trail.
func TestAuditSettingsUpdateRecordsSectionPath(t *testing.T) {
	deps := mountDeps(t)
	sink := &auditSink{}
	deps.Audit = audit.NewEmitter(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps.Stores.Settings = settings.NewStore(gen.New(execOnlyDBTX{}))

	r := chi.NewRouter()
	Mount(r, deps)

	body := `{"enabled":true,"allowUnauthenticated":false}`
	req := httptest.NewRequest(http.MethodPut, "/settings/proxy-mode", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	deps.Audit.Close()

	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Action != "settings.update" {
		t.Fatalf("action = %q, want settings.update", ev.Action)
	}
	if ev.Change == nil || !contains(ev.Change.Fields, "sections.proxy-mode") {
		t.Fatalf("change = %+v, want sections.proxy-mode", ev.Change)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func authPost(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
