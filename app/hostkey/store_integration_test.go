//go:build integration

// store_integration_test.go covers HostKey resolution and the default-account fill against a real Postgres. Run with: make test-integration.

package hostkey_test

import (
	"context"
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
				Spec: hostkey.Spec{HostID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: tc.env}},
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

// A key saved without an account joins its owner's default account at its host; keys of one owner share it, another owner gets its own, and a named account is kept.
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
	save := func(name string, owner meta.Owner, accountID string) *hostkey.HostKey {
		t.Helper()
		k := &hostkey.HostKey{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner},
			Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: accountID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
		}
		if err := store.Upsert(ctx, k); err != nil {
			t.Fatalf("Upsert %s: %v", name, err)
		}
		got, err := store.Get(ctx, k.Meta.ID)
		if err != nil || got == nil {
			t.Fatalf("Get %s: %v (row %v)", name, err, got)
		}
		if got.Spec.AccountID == "" {
			t.Fatalf("%s saved without an account", name)
		}
		return got
	}
	system := meta.Owner{Kind: meta.OwnerSystem}
	alice := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
	a := save("key-a", system, "")
	b := save("key-b", system, "")
	c := save("key-c", alice, "")
	d := save("key-d", system, named.Meta.ID)
	if a.Spec.AccountID != b.Spec.AccountID {
		t.Errorf("keys of one owner joined %q and %q, want one default account", a.Spec.AccountID, b.Spec.AccountID)
	}
	if c.Spec.AccountID == a.Spec.AccountID {
		t.Errorf("another owner's key joined the system default account")
	}
	if d.Spec.AccountID != named.Meta.ID {
		t.Errorf("named account = %q, want %q kept", d.Spec.AccountID, named.Meta.ID)
	}
	def, err := account.NewStore(q).Get(ctx, a.Spec.AccountID)
	if err != nil || def == nil || !def.Spec.Default || def.Meta.Owner != system || def.Spec.HostID != hst.Meta.ID {
		t.Fatalf("default account = %+v, %v", def, err)
	}

	// An update that drops the account joins the default again.
	d.Spec.AccountID = ""
	if err := store.Upsert(ctx, d); err != nil {
		t.Fatalf("Upsert without account: %v", err)
	}
	if got, _ := store.Get(ctx, d.Meta.ID); got == nil || got.Spec.AccountID != a.Spec.AccountID {
		t.Fatalf("key-d after dropping its account = %+v, want the default account", got)
	}
}
