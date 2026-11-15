//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
)

// The way back from migration 45: until `relay secret organization-to-system` has sealed the organization's host keys under the system data key again, `relay migrate down 44` is refused and the schema stays where it was; after it, the rollback runs and every key still opens.
func TestIntegration_OrganizationToSystemRoundTrip(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x42}, 32)
	dsn := storagetest.EmptyDB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("ACME_ORG_KEY", "sk-env")
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(ctx, dsn, storage.WithMigrateOnBoot(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sec, err := appsecret.Wire(st.Pool(), [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	keys := hostkey.NewStore(st.Pool(), sec.Registry, sec.Stored)
	org := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "org-stored", Owner: meta.Global},
		Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, Value: "sk-org"}}
	env := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: "org-env", Owner: meta.Global},
		Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_ORG_KEY"}}}
	for _, k := range []*hostkey.HostKey{org, env} {
		if err := keys.Upsert(ctx, k); err != nil {
			t.Fatalf("upsert %s: %v", k.Meta.Name, err)
		}
	}
	scope := func() string {
		t.Helper()
		row, err := gen.New(st.Pool()).GetSecretValue(ctx, org.Meta.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Scope.String
	}
	if got := scope(); got != stored.ScopeOrganization {
		t.Fatalf("value scope = %q, want organization", got)
	}

	if err := runMigrate([]string{"down", "44"}); err == nil {
		t.Fatal("relay migrate down 44 ran while organization-sealed values remained")
	}
	if v := versionIn(t, dsn, "schema_migrations"); v != int64(storagetest.LatestVersion(t)) {
		t.Fatalf("schema after the refused rollback = %d, want %d", v, storagetest.LatestVersion(t))
	}
	if err := runSecret([]string{"organization-to-system"}); err != nil {
		t.Fatalf("relay secret organization-to-system: %v", err)
	}
	got, err := keys.Get(ctx, org.Meta.ID)
	if err != nil || got == nil || got.Meta.Owner != (meta.Owner{Kind: meta.OwnerSystem}) || got.Resolved != "sk-org" || scope() != stored.ScopeSystem {
		t.Fatalf("after organization-to-system: %+v (%v), scope %q; want system-owned, sealed under system, opening to sk-org", got, err, scope())
	}
	if err := runMigrate([]string{"down", "44"}); err != nil {
		t.Fatalf("relay migrate down 44 after organization-to-system: %v", err)
	}
	for _, k := range []*hostkey.HostKey{org, env} {
		got, err := keys.Get(ctx, k.Meta.ID)
		if err != nil || got == nil || got.Meta.Owner != (meta.Owner{Kind: meta.OwnerSystem}) || got.Resolved == "" {
			t.Fatalf("%s at schema 44: %+v (%v), want system-owned and resolving", k.Meta.Name, got, err)
		}
	}
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
}
