package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/scim"
)

type scimAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *scimAuditSink) Write(_ context.Context, evs []audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evs...)
	return nil
}

func (s *scimAuditSink) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

// A SCIM request's writes become audit rows attributed to the SCIM client: one per write that ran or was refused for reaching a row the client does not own.
func TestSCIMWritesAreAuditedAsTheClient(t *testing.T) {
	sink := &scimAuditSink{}
	em := audit.NewEmitter(sink, slog.Default())
	writes := []scim.Write{
		{Client: "entra", Kind: "user", Op: "create", ID: "u-1", Name: "ada"},
		{Client: "entra", Kind: "user", Op: "disable", ID: "u-1", Name: "ada"},
		{Client: "entra", Kind: "group", Op: "update", ID: "g-1", Fields: []string{"members"}},
		{Client: "entra", Kind: "user", Op: "update", ID: "local", Err: fmt.Errorf("%w: %w", directory.ErrNotFound, directory.ErrForeign)},
		{Client: "entra", Kind: "user", Op: "create", Err: directory.ErrConflict},
		{Client: "entra", Kind: "user", Op: "delete", ID: "u-2", Err: fmt.Errorf("%w: last admin", scim.ErrRefused)},
	}
	h := audit.Middleware(em, nil)(auditSCIM(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, wr := range writes {
			collectSCIMWrite(r.Context(), wr)
		}
		w.WriteHeader(http.StatusOK)
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPatch, "/scim/v2/Users/u-1", nil))
	em.Close()

	type row struct{ action, status, field string }
	var got []row
	for _, ev := range sink.events {
		if ev.Actor.Kind != audit.ActorSCIMClient || ev.Actor.Name != "entra" {
			t.Fatalf("actor %+v", ev.Actor)
		}
		field := ""
		if ev.Change != nil && len(ev.Change.Fields) > 0 {
			field = ev.Change.Fields[0]
		}
		got = append(got, row{ev.Action, ev.Outcome.Status, field})
	}
	want := []row{
		{"users.create", audit.StatusAllowed, audit.AnyField},
		{"users.update", audit.StatusAllowed, "disabled"},
		{"groups.update", audit.StatusAllowed, "members"},
		{"users.update", audit.StatusDenied, ""},
		{"users.delete", audit.StatusDenied, audit.AnyField},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows %+v, want %+v", got, want)
	}
}

// A SCIM client may not disable or delete the last enabled admin; with another one enabled, or for any other account, it may.
func TestSCIMKeepsOneEnabledAdmin(t *testing.T) {
	ctx := context.Background()
	users := memstore.New().Stores().Users
	ada := &directory.User{ID: "u-ada", Username: "ada", Roles: []string{user.RoleAdmin}}
	bob := &directory.User{ID: "u-bob", Username: "bob"}
	for _, u := range []*directory.User{ada, bob} {
		if err := users.Upsert(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	check := keepOneAdminAgainstSCIM(users)
	if err := check(ctx, ada); !errors.Is(err, scim.ErrRefused) {
		t.Fatalf("the last enabled admin: %v, want a refusal", err)
	}
	if err := check(ctx, bob); err != nil {
		t.Fatalf("a member: %v", err)
	}
	bob.Roles = []string{user.RoleAdmin}
	if err := users.Upsert(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if err := check(ctx, ada); err != nil {
		t.Fatalf("with another enabled admin: %v", err)
	}
}

// Outside an audited request the collector has nowhere to put a write and drops it.
func TestSCIMWriteOutsideARequestIsDropped(t *testing.T) {
	collectSCIMWrite(context.Background(), scim.Write{Kind: "user", Op: "create", Err: errors.New("x")})
}
