//go:build integration

// store_integration_test.go covers HostKey resolution against a real Postgres: a stored row naming a reserved env var must load without its value. Run with: make test-integration.

package hostkey_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/secret"
)

// Upsert does not validate, so it writes the row an older relay could have stored.
func TestIntegration_StoredReservedEnvRefDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	store := hostkey.NewStore(gen.New(storagetest.Pool(t)), reg, nil)

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
				Spec: hostkey.Spec{HostID: meta.NewID(), PolicyID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: tc.env}},
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
