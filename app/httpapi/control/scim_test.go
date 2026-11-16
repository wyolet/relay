package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/scim"
	"github.com/wyolet/relay/pkg/meta"
)

// A SCIM request's writes become audit rows attributed to the SCIM client: one per write that ran or was refused for reaching a row the client does not own. A write that failed validation changed nothing and leaves none.
func TestSCIMWritesAreAuditedAsTheClient(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	writes := []scim.Write{
		{Client: "entra", Kind: "user", Op: "create", ID: "u-1", Name: "ada"},
		{Client: "entra", Kind: "user", Op: "disable", ID: "u-1", Name: "ada"},
		{Client: "entra", Kind: "user", Op: "enable", ID: "u-1", Name: "ada"},
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
	// A request that wrote nothing leaves no row.
	h = audit.Middleware(em, nil)(auditSCIM(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/scim/v2/Users", nil))
	em.Close()

	org := meta.Global
	event := func(action, status, kind, id, name string, fields ...string) audit.Event {
		return audit.Event{
			Actor: audit.Actor{Kind: audit.ActorSCIMClient, Name: "entra"}, Action: action,
			Resource: audit.Resource{Kind: kind, ID: id, Name: name, Owner: &org},
			Outcome:  audit.Outcome{Status: status, Code: http.StatusOK}, Request: audit.Request{Method: http.MethodPatch, Path: "/scim/v2/Users/u-1"},
			Change: &audit.Change{Fields: fields},
		}
	}
	wantEvents(t, sink.all(),
		event("users.create", audit.StatusAllowed, "user", "u-1", "ada", audit.AnyField),
		event("users.update", audit.StatusAllowed, "user", "u-1", "ada", "disabled"),
		event("users.update", audit.StatusAllowed, "user", "u-1", "ada", "disabled"),
		event("groups.update", audit.StatusAllowed, "group", "g-1", "", "members"),
		event("users.update", audit.StatusDenied, "user", "local", ""),
		event("users.delete", audit.StatusDenied, "user", "u-2", "", audit.AnyField),
	)
}

// A SCIM client may not disable or delete the last enabled admin; with another one enabled, or for any other account, it may.
func TestSCIMKeepsOneEnabledAdmin(t *testing.T) {
	ctx := context.Background()
	users := memstore.New().Stores().Users
	ada := &directory.User{ID: "u-ada", Username: "ada", Roles: []string{user.RoleAdmin}}
	bob := &directory.User{ID: "u-bob", Username: "bob"}
	check := keepOneAdminAgainstSCIM(users)
	for _, step := range []struct {
		name     string
		bob      directory.User
		target   *directory.User
		wantLast bool
	}{
		{"the only admin", *bob, ada, true},
		{"a member", *bob, bob, false},
		{"the other admin is disabled", directory.User{ID: "u-bob", Username: "bob", Roles: []string{user.RoleAdmin}, Disabled: true}, ada, true},
		{"a disabled admin", directory.User{ID: "u-bob", Username: "bob", Roles: []string{user.RoleAdmin}, Disabled: true}, &directory.User{ID: "u-bob", Roles: []string{user.RoleAdmin}, Disabled: true}, false},
		{"another admin is enabled", directory.User{ID: "u-bob", Username: "bob", Roles: []string{user.RoleAdmin}}, ada, false},
	} {
		for _, u := range []*directory.User{ada, &step.bob} {
			if err := users.Upsert(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		err := check(ctx, step.target)
		if step.wantLast && (!errors.Is(err, scim.ErrRefused) || err.Error() != scim.ErrRefused.Error()+": this is the last enabled admin; grant the role to another account first") {
			t.Errorf("%s: %v, want the last-admin refusal", step.name, err)
		}
		if !step.wantLast && err != nil {
			t.Errorf("%s: %v, want it allowed", step.name, err)
		}
	}
}

// Outside an audited request the collector has nowhere to put a write and drops it.
func TestSCIMWriteOutsideARequestIsDropped(t *testing.T) {
	collectSCIMWrite(context.Background(), scim.Write{Kind: "user", Op: "create", Err: errors.New("x")})
}
