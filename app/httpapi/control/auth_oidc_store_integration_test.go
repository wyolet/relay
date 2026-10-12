//go:build integration

package control

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/oidc"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// Sign-in resolves accounts through the auth module's identities table: an account a binary from before that table wrote is found, a first sign-in provisions an account that the next sign-in finds again, a first sign-in with a taken email is refused, and the other refusals are unchanged.
func TestOIDCSignInOverIdentities(t *testing.T) {
	ctx := t.Context()
	pool := storagetest.Pool(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	cfg := &settings.AuthOIDC{Enabled: true, Issuer: "https://idp.example.com", ClientID: "c", Registration: "open"}
	ops := newDirectoryOps(Deps{Stores: stores})
	od := newOIDC(func() *settings.AuthOIDC { return cfg }, func() *settings.AuthSCIM { return &settings.AuthSCIM{} }, directoryUsers{Store: stores.Users, ops: ops}, &fakeSessions{}, false)
	claims := func(sub, username string) oidc.Claims {
		return oidc.Claims{Issuer: cfg.Issuer, Subject: sub, PreferredUsername: username}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('u-old', 'ada', 'https://idp.example.com|sub-ada')`); err != nil {
		t.Fatal(err)
	}
	if u, err := od.resolveUser(ctx, claims("sub-ada", "ada")); err != nil || u.ID != "u-old" {
		t.Fatalf("sign-in of an account written old-style = %+v, %v", u, err)
	}

	first, err := od.resolveUser(ctx, claims("sub-new", "ada"))
	if err != nil || first.Username != "ada-2" || first.OIDCSubject != "https://idp.example.com|sub-new" {
		t.Fatalf("first sign-in = %+v, %v", first, err)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer = $1 AND subject = 'sub-new'`, cfg.Issuer).Scan(&owner); err != nil || owner != first.ID {
		t.Fatalf("identity of the provisioned account = %q, %v", owner, err)
	}
	if again, err := od.resolveUser(ctx, claims("sub-new", "other")); err != nil || again.ID != first.ID {
		t.Fatalf("second sign-in = %+v, %v; want %s", again, err, first.ID)
	}

	dir, err := ops.directory()
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.AddIdentity(ctx, "u-old", directory.UserIdentity{Identity: directory.Identity{Issuer: cfg.Issuer, Subject: "sub-ada-2"}}); err != nil {
		t.Fatal(err)
	}
	if u, err := od.resolveUser(ctx, claims("sub-ada-2", "")); err != nil || u.ID != "u-old" || u.OIDCSubject != "https://idp.example.com|sub-ada" {
		t.Fatalf("sign-in by a second identity = %+v, %v", u, err)
	}

	if err := dir.CreateUser(ctx, nil, &directory.User{Username: "grace", Email: "grace@example.com"}); err != nil {
		t.Fatal(err)
	}
	taken := claims("sub-grace", "grace")
	taken.Email = "grace@example.com"
	wantFailure(t, "email of an account with no identity", od, taken, http.StatusForbidden, "an account with this email already exists; an administrator must link it to this sign-in")

	cfg.Registration = "closed"
	wantFailure(t, "closed registration", od, claims("sub-unknown", "x"), http.StatusForbidden, "no account for this identity (registration is closed)")
	if err := dir.DisableUser(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, "disabled account", od, claims("sub-new", ""), http.StatusForbidden, "account disabled")
}

// An account a binary from before the identities table wrote under a configured issuer with a trailing slash signs in after the upgrade whichever form is configured, and that binary's exact oidc_subject lookup still finds it and the accounts provisioned since.
func TestOIDCSignInAfterIssuerSlashUpgrade(t *testing.T) {
	ctx := t.Context()
	dsn := storagetest.DB(t)
	if err := authpg.MigrateDown(dsn, 1); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const slashed = "https://idp.example.com/"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, username, oidc_subject) VALUES ('u-old', 'ada', $1)`, slashed+"|sub-ada"); err != nil {
		t.Fatal(err)
	}
	if err := authpg.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	cfg := &settings.AuthOIDC{Enabled: true, ClientID: "c", Registration: "closed"}
	od := newOIDC(func() *settings.AuthOIDC { return cfg }, func() *settings.AuthSCIM { return &settings.AuthSCIM{} },
		directoryUsers{Store: stores.Users, ops: newDirectoryOps(Deps{Stores: stores})}, &fakeSessions{}, false)

	for _, issuer := range []string{slashed, "https://idp.example.com"} {
		cfg.Issuer = issuer
		if u, err := od.resolveUser(ctx, oidc.Claims{Issuer: issuer, Subject: "sub-ada"}); err != nil || u.ID != "u-old" {
			t.Fatalf("sign-in through %s = %+v, %v; want u-old", issuer, u, err)
		}
	}

	cfg.Issuer, cfg.Registration = slashed, "open"
	grace, err := od.resolveUser(ctx, oidc.Claims{Issuer: slashed, Subject: "sub-grace", PreferredUsername: "grace"})
	if err != nil || grace.OIDCSubject != slashed+"|sub-grace" {
		t.Fatalf("first sign-in = %+v, %v; want the configured issuer kept", grace, err)
	}
	for subject, want := range map[string]string{slashed + "|sub-ada": "u-old", slashed + "|sub-grace": grace.ID} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE oidc_subject = $1`, subject).Scan(&got); err != nil || got != want {
			t.Fatalf("rollback lookup of %s = %q, %v; want %q", subject, got, err, want)
		}
	}
}

func wantFailure(t *testing.T, what string, od *oidcDeps, c oidc.Claims, status int, message string) {
	t.Helper()
	_, err := od.resolveUser(t.Context(), c)
	f, ok := errors.AsType[*oidc.Failure](err)
	if !ok || f.Status != status || f.Message != message {
		t.Fatalf("%s: err = %v, want %d %q", what, err, status, message)
	}
}
