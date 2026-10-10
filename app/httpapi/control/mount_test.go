package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/kv"
)

type fakeAuditReader struct{}

func (fakeAuditReader) Events(context.Context, audit.Query) ([]audit.Event, error) {
	return nil, nil
}

type fakeDenylist struct{}

func (fakeDenylist) Set(context.Context, string, []byte, time.Duration) error { return nil }

type fakePayloadReader struct{}

func (fakePayloadReader) Get(context.Context, string) (payloadlog.Record, error) {
	return payloadlog.Record{}, payloadlog.ErrNotFound
}

type fakeHostHealth struct{}

func (fakeHostHealth) Read(context.Context, string) (host.Status, bool) { return host.Status{}, false }
func (fakeHostHealth) ReadAll(context.Context) map[string]host.Status   { return nil }

// mountDeps is every field Mount reads, populated so that no register*
// short-circuits on a nil dependency. Stores are constructed against a nil
// pool: mounting never touches the database.
func mountDeps(t *testing.T) Deps {
	t.Helper()
	var pool *pgxpool.Pool
	q := gen.New(pool)
	kvStore := kv.NewMem()
	t.Cleanup(func() { _ = kvStore.Close() })

	secReg, secStored := appsecret.Wire(q, pool, nil)
	stores := &appcatalog.Stores{
		Provider:       provider.NewStore(q),
		Host:           host.NewStore(q),
		Model:          model.NewStore(q),
		HostKey:        hostkey.NewStore(q, secReg, secStored),
		RateLimit:      ratelimit.NewStore(q),
		Policy:         policy.NewStore(pool),
		Pricing:        pricing.NewStore(pool),
		Binding:        binding.NewStore(pool),
		Key:            key.NewStore(q),
		Overlay:        overlay.NewStore(q),
		Settings:       settings.NewStore(q),
		Team:           team.NewStore(q),
		Project:        project.NewStore(q),
		ServiceAccount: serviceaccount.NewStore(q),
		Group:          group.NewStore(pool),
		Role:           role.NewStore(q),
		RoleBinding:    rolebinding.NewStore(pool),
		PolicyBinding:  policybinding.NewStore(pool),
		Secrets:        secReg,
		Stored:         secStored,
	}
	cat := appcatalog.New(stores.Provider, stores.Host, stores.Policy, stores.Model,
		stores.HostKey, stores.RateLimit, stores.Key, stores.Pricing, stores.Binding)
	cat.UseTenancy(stores.Team, stores.Project, stores.ServiceAccount, stores.Group,
		stores.Role, stores.RoleBinding, stores.PolicyBinding)
	sink := &auditSink{}
	return Deps{
		Users:         user.NewStore(q),
		Sessions:      session.New(kvStore, false, "sess:"),
		TokenSigner:   &TokenSigner{},
		TokenDenylist: fakeDenylist{},
		AdminToken:    "test-admin-token",
		Authz:         audit.Authorizer{Inner: authz.AlwaysAllowAuthenticated{}, Snap: cat.Current},
		Catalog:       cat,
		Stores:        stores,
		UsageReader:   &fakeUsageReader{},
		Audit:         audit.NewEmitter(sink, slog.New(slog.DiscardHandler)),
		AuditReader:   fakeAuditReader{},
		PayloadReader: fakePayloadReader{},
		Selector:      keypool.New(kvStore, slog.New(slog.DiscardHandler), nil, nil),
		HostHealth:    fakeHostHealth{},
	}
}

// Mounting the whole control API is what catches a schema-registry collision
// between two packages that both export a type of the same name (huma panics
// with "duplicate name: X" at registration, which no per-handler test sees).
func TestMountRegistersEveryOperation(t *testing.T) {
	r := chi.NewRouter()
	api := Mount(r, mountDeps(t))
	if api == nil {
		t.Fatal("Mount returned no api")
	}

	// The generated document is where the registry's names surface.
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"AuditEvent", "Event", "audit_list", "usage_events", "list_users"} {
		if !strings.Contains(body, want) {
			t.Errorf("openapi.json does not mention %q", want)
		}
	}
}

// Every entity update declares the 409 a stale resourceVersion answers with,
// and Metadata carries the field clients send back, so a generated client
// can handle both without reading prose.
func TestOpenAPIDeclaresResourceVersionConflicts(t *testing.T) {
	api := Mount(chi.NewRouter(), mountDeps(t))
	doc := api.OpenAPI()

	updates := 0
	for path, item := range doc.Paths {
		op := item.Put
		// Users carry no Metadata: their update is a partial patch of the
		// fields sent, outside this contract.
		if op == nil || !strings.HasPrefix(op.OperationID, "update_") || !strings.Contains(path, "/by-id/") ||
			op.OperationID == "update_user" {
			continue
		}
		updates++
		if _, ok := op.Responses["409"]; !ok {
			t.Errorf("%s (%s) does not declare 409", op.OperationID, path)
		}
	}
	if updates < 16 {
		t.Fatalf("found %d entity update operations, want at least 16", updates)
	}

	md := doc.Components.Schemas.Map()["Metadata"]
	if md == nil {
		t.Fatal("no Metadata schema")
	}
	if _, ok := md.Properties["resourceVersion"]; !ok {
		t.Fatal("Metadata schema has no resourceVersion")
	}
}

// The control API is mounted in front of the SPA fallback, which serves
// HTML for anything it does not recognise. An unknown path under the API
// answers a parseable 404 — an older UI calling a renamed endpoint must see
// an error, not an index page.
func TestUnknownControlRouteAnswersJSON(t *testing.T) {
	deps := mountDeps(t)
	// The composition root's wiring: the API under /api, the SPA as the
	// listener-wide fallback behind it.
	root := chi.NewRouter()
	root.Route("/api", func(r chi.Router) { Mount(r, deps) })
	root.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>spa</html>"))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/relay-keys", nil)
	req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
	w := httptest.NewRecorder()
	root.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Type, Code, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, w.Body.String())
	}
	if body.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", body.Error.Code)
	}
}
