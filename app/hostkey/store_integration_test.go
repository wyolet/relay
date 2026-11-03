//go:build integration

// store_integration_test.go covers HostKey resolution and the account link against a real Postgres. Run with: make test-integration.

package hostkey_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

// Upsert does not validate, so it writes the row an older relay could have stored.
func TestIntegration_StoredReservedEnvRefDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	store := hostkey.NewStore(storagetest.Pool(t), reg, nil)

	t.Setenv("RELAY_MASTER_KEY", "relay-config-value")
	t.Setenv("ACME_API_KEY", "sk-acme")
	for _, tc := range []struct {
		env, want string
		resolves  bool
	}{
		{env: "RELAY_MASTER_KEY"},
		{env: "ACME_API_KEY", want: "sk-acme", resolves: true},
	} {
		t.Run(tc.env, func(t *testing.T) {
			id := meta.NewID()
			k := &hostkey.HostKey{
				Meta: meta.Metadata{ID: id, Name: "env-key-" + id[len(id)-8:], Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: tc.env}},
			}
			if err := store.Upsert(ctx, k); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			got, err := store.Get(ctx, id)
			if err != nil || got == nil {
				t.Fatalf("Get: %v (row %v)", err, got)
			}
			if got.Resolved != tc.want {
				t.Errorf("Resolved = %q, want %q", got.Resolved, tc.want)
			}
			if (got.Status.Unresolved == nil) != tc.resolves {
				t.Fatalf("Unresolved = %+v, want set = %v", got.Status.Unresolved, !tc.resolves)
			}
			if !tc.resolves && got.Status.Unresolved.Reason != hostkey.ErrReservedEnv.Error() {
				t.Errorf("reason = %q, want %q", got.Status.Unresolved.Reason, hostkey.ErrReservedEnv)
			}
		})
	}
}

// The account link rides in the spec JSONB beside the tier policy.
func TestIntegration_AccountIDRoundTrips(t *testing.T) {
	ctx := context.Background()
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	store := hostkey.NewStore(storagetest.Pool(t), reg, nil)
	t.Setenv("ACME_API_KEY", "sk-acme")
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "account-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
	}
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, k.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if got.Spec.AccountID != k.Spec.AccountID {
		t.Fatalf("accountId = %q, want %q", got.Spec.AccountID, k.Spec.AccountID)
	}
}

// A key saved without an account is refused, on create and on update: nothing makes an account for it. A named account is kept.
func TestIntegration_SavedKeyAlwaysHasAnAccount(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	q := gen.New(pool)
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	store := hostkey.NewStore(pool, reg, nil)
	t.Setenv("ACME_API_KEY", "sk-acme")
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	named := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-enterprise", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: account.Spec{HostID: hst.Meta.ID},
	}
	if err := account.NewStore(q).Upsert(ctx, named); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "key-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
	}
	if err := store.Upsert(ctx, k); !errors.Is(err, hostkey.ErrNoAccount) {
		t.Fatalf("Upsert without an account = %v, want ErrNoAccount", err)
	}
	if got, _ := store.Get(ctx, k.Meta.ID); got != nil {
		t.Fatalf("key saved without an account: %+v", got)
	}
	if accounts, _ := account.NewStore(q).List(ctx); len(accounts) != 1 {
		t.Fatalf("accounts after the refused save = %d, want only the named one", len(accounts))
	}

	k.Spec.AccountID = named.Meta.ID
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, k.Meta.ID)
	if err != nil || got == nil || got.Spec.AccountID != named.Meta.ID {
		t.Fatalf("Get = %+v, %v; want account %q", got, err, named.Meta.ID)
	}

	// An update that omits the account is refused too, and leaves the stored one.
	got.Spec.AccountID = ""
	got.Meta.DisplayName = "renamed"
	if err := store.Upsert(ctx, got); !errors.Is(err, hostkey.ErrNoAccount) {
		t.Fatalf("update without an account = %v, want ErrNoAccount", err)
	}
	if got, _ := store.Get(ctx, k.Meta.ID); got == nil || got.Spec.AccountID != named.Meta.ID || got.Meta.DisplayName == "renamed" {
		t.Fatalf("key after the refused update = %+v, want it unchanged", got)
	}
}
