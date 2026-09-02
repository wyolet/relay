//go:build integration

// policyref_integration_test.go exercises checkPolicyRefVisible and
// checkHostKeyRefsVisible against a real Postgres: both read through
// concrete *policy.Store / *hostkey.Store, which have no fake seam. Run
// with: make test-integration (or RELAY_TEST_PG_DSN + `go test -tags=integration`).
package control

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func setupPolicyRefDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	_ = m.Drop() // tolerate "no schema" on first run
	src2, _ := iofs.New(pgmigrations.FS, ".")
	m2, err := migrate.NewWithSourceInstance("iofs", src2, dsn)
	if err != nil {
		t.Fatalf("migrate re-init: %v", err)
	}
	if err := m2.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// visibleCtx carries an admin actor so RBAC.Visible bypasses unconditionally
// — the guards under test decide on ownership, not on the caller's own
// scope, so an admin context isolates that decision from RBAC binding setup.
func visibleCtx(ctx context.Context) context.Context {
	return actor.WithActor(ctx, &actor.Actor{AdminToken: true})
}

// TestCheckPolicyRefVisible_PersonalRowCannotReferenceProjectPolicy covers
// the three cases from the guard's contract: a user-owned row may not
// point at a project-owned policy, but project-owned or same-user-owned
// references are both allowed through.
func TestCheckPolicyRefVisible_PersonalRowCannotReferenceProjectPolicy(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "policyref-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	projectPolicy := &policy.Policy{Meta: meta.Metadata{
		ID: meta.NewID(), Name: "project-policy",
		Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()},
	}}
	userPolicy := &policy.Policy{Meta: meta.Metadata{
		ID: meta.NewID(), Name: "user-policy",
		Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"},
	}}
	for _, p := range []*policy.Policy{projectPolicy, userPolicy} {
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert policy %s: %v", p.Meta.Name, err)
		}
	}

	// An outsider — someone who may not create keys in that project — is
	// still refused; a member of the project is not (their personal key
	// then carries the project's attribution).
	outsider := actor.WithActor(ctx, &actor.Actor{UserID: "u-alice", Username: "alice"})

	t.Run("user-owned key referencing a foreign project's policy is rejected", func(t *testing.T) {
		err := checkPolicyRefVisible(outsider, d, projectPolicy.Meta.ID, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		// Refused either as invisible or as a personal row reaching into a
		// project, depending on what the caller may see; both are 400s that
		// keep the reference out.
		got := err.Error()
		if !strings.Contains(got, "personal rows cannot reference project resources") &&
			!strings.Contains(got, "not found") {
			t.Fatalf("err = %q, want the reference refused", got)
		}
	})

	t.Run("user-owned key referencing a project the caller may create keys in is allowed", func(t *testing.T) {
		if err := checkPolicyRefVisible(visibleCtx(ctx), d, projectPolicy.Meta.ID, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}); err != nil {
			t.Fatalf("checkPolicyRefVisible: %v", err)
		}
	})

	t.Run("project-owned key referencing a project-owned policy is allowed", func(t *testing.T) {
		if err := checkPolicyRefVisible(visibleCtx(ctx), d, projectPolicy.Meta.ID, meta.Owner{Kind: meta.OwnerProject, ID: projectPolicy.Meta.Owner.ID}); err != nil {
			t.Fatalf("checkPolicyRefVisible: %v", err)
		}
	})

	t.Run("user-owned key referencing a user-owned policy is allowed", func(t *testing.T) {
		if err := checkPolicyRefVisible(visibleCtx(ctx), d, userPolicy.Meta.ID, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}); err != nil {
			t.Fatalf("checkPolicyRefVisible: %v", err)
		}
	})
}

// TestCheckHostKeyRefsVisible_PersonalRowCannotReferenceProjectHostKey
// mirrors the policy case for the HostKey side of the same guard.
func TestCheckHostKeyRefsVisible_PersonalRowCannotReferenceProjectHostKey(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}

	// The env-mode HostKey.Get round-trips through secret.Resolve, which
	// errors (and the guard silently skips the row) if the var is unset.
	t.Setenv("POLICYREF_TEST_KEY", "sk-test-value")

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "policyref-hostkey-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	projectHostKey := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "project-hostkey", Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "POLICYREF_TEST_KEY"}},
	}
	userHostKey := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "user-hostkey", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "POLICYREF_TEST_KEY"}},
	}
	for _, k := range []*hostkey.HostKey{projectHostKey, userHostKey} {
		if err := stores.HostKey.Upsert(ctx, k); err != nil {
			t.Fatalf("upsert host-key %s: %v", k.Meta.Name, err)
		}
	}

	t.Run("user-owned key referencing a project-owned host-key is rejected", func(t *testing.T) {
		err := checkHostKeyRefsVisible(visibleCtx(ctx), d, []string{projectHostKey.Meta.ID}, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		if got := err.Error(); !strings.Contains(got, "personal rows cannot reference project resources") {
			t.Fatalf("err = %q, want it to mention personal rows cannot reference project resources", got)
		}
	})

	t.Run("project-owned key referencing a project-owned host-key is allowed", func(t *testing.T) {
		if err := checkHostKeyRefsVisible(visibleCtx(ctx), d, []string{projectHostKey.Meta.ID}, meta.Owner{Kind: meta.OwnerProject, ID: projectHostKey.Meta.Owner.ID}); err != nil {
			t.Fatalf("checkHostKeyRefsVisible: %v", err)
		}
	})

	t.Run("user-owned key referencing a user-owned host-key is allowed", func(t *testing.T) {
		if err := checkHostKeyRefsVisible(visibleCtx(ctx), d, []string{userHostKey.Meta.ID}, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}); err != nil {
			t.Fatalf("checkHostKeyRefsVisible: %v", err)
		}
	})
}
