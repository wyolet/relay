package control

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// denyAll refuses everything an authenticated caller asks for.
type denyAll struct{}

func (denyAll) Authorize(context.Context, string, authz.Resource) error {
	return authz.ErrForbidden
}

// A disabled account keeps no tokens, so it must not be able to sign a new
// one either.
func TestMintRefusesADisabledAccount(t *testing.T) {
	f := newTokenFixture(t)
	f.users[f.userID].Disabled = true
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	_, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
	if got := statusOf(t, err); got != 403 {
		t.Fatalf("status = %d, want 403", got)
	}
}

// A project the caller may not mint for answers exactly like one that does
// not exist; a 403 would enumerate project slugs for anyone with a session.
func TestMintHidesProjectsTheCallerCannotSee(t *testing.T) {
	f := newTokenFixture(t)
	f.deps.authz = denyAll{}
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	_, forbidden := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
	_, unknown := mintToken(ctx, f.deps, mintBody("no-such-project", ""))
	if statusOf(t, forbidden) != 404 || statusOf(t, unknown) != 404 {
		t.Fatalf("forbidden = %d, unknown = %d, want both 404",
			statusOf(t, forbidden), statusOf(t, unknown))
	}
	if forbidden.Error() != unknown.Error() {
		t.Errorf("bodies differ: %q vs %q", forbidden.Error(), unknown.Error())
	}
}

// Minting signs a long-lived credential off a session; the window is a floor
// under abuse, not a quota.
func TestMintIsRateLimitedPerUser(t *testing.T) {
	f := newTokenFixture(t)
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	f.deps.limiter = pkgratelimit.New(store, slog.Default(), nil)
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	for i := 0; i < mintLimitBudget; i++ {
		if _, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, "")); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	_, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
	if got := statusOf(t, err); got != 429 {
		t.Fatalf("status past the budget = %d, want 429", got)
	}

	// The window is per user: another account is unaffected.
	other := "u-other"
	f.users[other] = f.users[f.userID]
	otherCtx := actor.WithActor(context.Background(), &actor.Actor{UserID: other, Username: "bob"})
	if _, err := mintToken(otherCtx, f.deps, mintBody(f.project.Meta.Name, "")); err != nil {
		t.Fatalf("another user must have their own window: %v", err)
	}

	entries, err := store.Range(context.Background(), mintLimitKeyPrefix(f.userID))
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("no kv key under %q", mintLimitKeyPrefix(f.userID))
	}
}

// The hash tag is what keeps one user's window on one Redis Cluster slot.
func TestMintLimitKeyFormat(t *testing.T) {
	if got := mintLimitScope("u-1"); got != "user:u-1" {
		t.Errorf("scope = %q, want user:u-1", got)
	}
	got := mintLimitKeyPrefix("u-1")
	if !strings.HasPrefix(got, "limit:{user:u-1}:") {
		t.Errorf("key prefix = %q, want a {user:<id>} hash tag", got)
	}
	if !strings.Contains(got, ":"+mintLimitRule+":") {
		t.Errorf("key prefix = %q, want the %q rule segment", got, mintLimitRule)
	}
}

// A schema path is two segments; ".." survives path.Base, so the guard has to
// validate the whole path.
func TestSchemaPathRejectsTraversal(t *testing.T) {
	srv := schemaServer(t)
	for _, path := range []string{
		"/api/schemas/v1alpha2/..",
		"/api/schemas/..",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	resp, err := http.Get(srv.URL + "/api/schemas/" + manifest.SchemaVersion + "/Role")
	if err != nil {
		t.Fatalf("GET a real schema: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a real schema = %d, want 200", resp.StatusCode)
	}
}
