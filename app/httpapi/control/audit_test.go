package control

import (
	"context"
	"errors"
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
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/meta"
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

// wantEvents compares recorded events with want, leaving out what a request stamps on its own: ids, the time and the client address.
func wantEvents(t *testing.T, got []audit.Event, want ...audit.Event) {
	t.Helper()
	diff := cmp.Diff(want, got, cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(audit.Event{}, "ID", "TS"),
		cmpopts.IgnoreFields(audit.Actor{}, "IP", "SessionID"),
		cmpopts.IgnoreFields(audit.Request{}, "ID"))
	if diff != "" {
		t.Fatalf("audit events (-want +got):\n%s", diff)
	}
}

// newAuditHarness mounts one CRUD kind behind the audit middleware and the audit Authorizer wrapper.
func newAuditHarness(t *testing.T, inner authz.Authorizer, seed ...*scopedThing) (http.Handler, *auditSink, *audit.Emitter) {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	store := newMemStore(thingMeta, seed...)
	r, api := testAPI(t, scopeActors, audit.Middleware(em, nil))
	mountKind[scopedThing](api, "rate-limits", "rate-limit", store, audit.Authorizer{Inner: inner}, thingMeta, nil, meta.OwnerUser, nil, noSettings{})
	return r, sink, em
}

// A write leaves one row whether it was allowed or refused, naming the caller, the action, the target and the status; an allowed read leaves none.
func TestAuditCRUD(t *testing.T) {
	bob := audit.Actor{Kind: audit.ActorUser, ID: "u-bob", Name: "bob"}
	alice := audit.Actor{Kind: audit.ActorUser, ID: "u-alice", Name: "alice"}
	row := func(id string, owner meta.Owner) audit.Resource {
		return audit.Resource{Kind: "rate-limit", ID: id, Owner: &owner}
	}
	hostOwned, alices := meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	byID := func(id string) string { return "/rate-limits/by-id/" + id }

	for _, tc := range []struct {
		name, who, method, path, body string
		want                          []audit.Event
	}{
		// A catalog row is visible to everyone and mutable by admins only, so the authorizer itself denies.
		{"denied by the authorizer", "bob", "DELETE", byID(catalogID), "", []audit.Event{{
			Actor: bob, Action: "rate-limits.delete", Resource: row(catalogID, hostOwned),
			Outcome: audit.Outcome{Status: audit.StatusDenied, Code: 403}, Request: audit.Request{Method: "DELETE", Path: byID(catalogID)},
		}}},
		// Another user's row is invisible, so the handler answers 404 before any Authorize call and the route alone names the action.
		{"refused as not visible", "bob", "DELETE", byID(aliceID), "", []audit.Event{{
			Actor: bob, Action: "rate-limits.delete", Resource: audit.Resource{Kind: "rate-limit", ID: aliceID},
			Outcome: audit.Outcome{Status: audit.StatusDenied, Code: 404}, Request: audit.Request{Method: "DELETE", Path: byID(aliceID)},
		}}},
		{"allowed update names the changed paths", "alice", "PUT", byID(aliceID), `{"metadata":{"name":"alice-row","displayName":"Renamed"}}`, []audit.Event{{
			Actor: alice, Action: "rate-limits.update", Resource: row(aliceID, alices),
			Outcome: audit.Outcome{Status: audit.StatusAllowed, Code: 200}, Request: audit.Request{Method: "PUT", Path: byID(aliceID)},
			Change: &audit.Change{Fields: []string{"metadata.displayName"}},
		}}},
		{"allowed delete", "alice", "DELETE", byID(aliceID), "", []audit.Event{{
			Actor: alice, Action: "rate-limits.delete", Resource: row(aliceID, alices),
			Outcome: audit.Outcome{Status: audit.StatusAllowed, Code: 204}, Request: audit.Request{Method: "DELETE", Path: byID(aliceID)},
			Change: &audit.Change{Fields: []string{audit.AnyField}},
		}}},
		{"allowed list", "alice", "GET", "/rate-limits", "", nil},
		{"allowed get", "alice", "GET", "/rate-limits/" + aliceID, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sink, em := newAuditHarness(t, testRBAC(), seedThings()...)
			scopeReq(t, h, tc.who, tc.method, tc.path, tc.body)
			em.Close()
			wantEvents(t, sink.all(), tc.want...)
		})
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
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })
	sessions := session.New(kvStore, false)

	r := chi.NewRouter()
	r.Use(sessions.Middleware)
	r.Use(audit.Middleware(em, nil))
	api := humachi.New(r, huma.DefaultConfig("auth-audit-test", "0"))
	registerAuth(api, Deps{Identity: idStore, Sessions: sessions})
	return r, sink, em
}

// Login and logout record who tried: the user on success, anonymous carrying the attempted username on a wrong password, anonymous for a logout with no live session.
func TestAuditAuth(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
		want             audit.Event
	}{
		{"login", "/auth/login", `{"username":"alice","password":"secret123"}`, 200, audit.Event{
			Actor: audit.Actor{Kind: audit.ActorUser, ID: "u-alice", Name: "alice"}, Action: "auth.login",
			Resource: audit.Resource{Kind: "user", ID: "u-alice", Name: "alice"},
			Outcome:  audit.Outcome{Status: audit.StatusAllowed, Code: 200}, Request: audit.Request{Method: "POST", Path: "/auth/login"},
		}},
		{"login with a wrong password", "/auth/login", `{"username":"alice","password":"wrong"}`, 401, audit.Event{
			Actor: audit.Actor{Kind: audit.ActorAnonymous, Name: "alice"}, Action: "auth.login",
			Resource: audit.Resource{Kind: "user", Name: "alice"},
			Outcome:  audit.Outcome{Status: audit.StatusDenied, Code: 401}, Request: audit.Request{Method: "POST", Path: "/auth/login"},
		}},
		{"logout with no session", "/auth/logout", "", 204, audit.Event{
			Actor: audit.Actor{Kind: audit.ActorAnonymous}, Action: "auth.logout",
			Resource: audit.Resource{Kind: "user"},
			Outcome:  audit.Outcome{Status: audit.StatusAllowed, Code: 204}, Request: audit.Request{Method: "POST", Path: "/auth/logout"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sink, em := newAuthAuditHarness(t)
			if w := authPost(t, h, tc.path, tc.body); w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			em.Close()
			wantEvents(t, sink.all(), tc.want)
		})
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
	deps.Audit = audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
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
	wantEvents(t, sink.all(), audit.Event{
		Actor: audit.Actor{Kind: audit.ActorAdminToken, Name: "admin-token"}, Action: "settings.update",
		Resource: audit.Resource{Kind: "settings", Name: "proxy-mode"},
		Outcome:  audit.Outcome{Status: audit.StatusAllowed, Code: 200}, Request: audit.Request{Method: "PUT", Path: "/settings/proxy-mode"},
		Change: &audit.Change{Fields: []string{"sections.proxy-mode"}},
	})
}

func authPost(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
