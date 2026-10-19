//go:build integration

// scope_integration_test.go covers how a stored host key's value follows its owner: sealed under the owner's scope, moved in the same transaction when the owner changes, and unreadable once ciphertext or ownership is tampered with. Run with: make test-integration.

package hostkey_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	appsecret "github.com/wyolet/relay/app/secret"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

var masterKey = bytes.Repeat([]byte{0x5a}, 32)

func hostKeyStore(t *testing.T, pool *pgxpool.Pool, key []byte) (*hostkey.Store, *appsecret.Secrets) {
	t.Helper()
	sec, err := appsecret.Wire(pool, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	return hostkey.NewStore(pool, sec.Registry, sec.Stored), sec
}

func storedKey(owner meta.Owner, kind hostkey.ValueKind, value string) *hostkey.HostKey {
	id := meta.NewID()
	return &hostkey.HostKey{
		Meta: meta.Metadata{ID: id, Name: "key-" + id[len(id)-8:], Owner: owner},
		Spec: hostkey.Spec{HostID: meta.NewID(), PolicyID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: kind, Provider: "acme"}, Value: value},
	}
}

func scopeOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), "SELECT scope FROM secret_values WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, s *hostkey.Store, id string) *hostkey.HostKey {
	t.Helper()
	k, err := s.Get(context.Background(), id)
	if err != nil || k == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return k
}

func TestIntegration_OwnerChangeRescopes(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	store, _ := hostKeyStore(t, pool, masterKey)
	user := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
	project := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}

	k := storedKey(user, hostkey.ValueKindStored, "sk-moving")
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got := scopeOf(t, pool, k.Meta.ID); got != "user:"+user.ID {
		t.Fatalf("scope = %q", got)
	}

	// A metadata-only update that changes the owner moves the value.
	moved := get(t, store, k.Meta.ID)
	moved.Meta.Owner = project
	if err := store.Upsert(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if got := scopeOf(t, pool, k.Meta.ID); got != "project:"+project.ID {
		t.Fatalf("scope after owner change = %q", got)
	}
	if got := get(t, store, k.Meta.ID); got.Resolved != "sk-moving" || got.Status.Unresolved != nil {
		t.Fatalf("after owner change: %q %+v", got.Resolved, got.Status.Unresolved)
	}

	// The owner change and the rescope commit together: a process that cannot read the value fails the update and leaves the owner as it was.
	badStore, _ := hostKeyStore(t, pool, bytes.Repeat([]byte{0x11}, 32))
	again := get(t, store, k.Meta.ID)
	again.Meta.Owner = user
	if err := badStore.Upsert(ctx, again); err == nil {
		t.Fatal("an owner change whose rescope failed was accepted")
	}
	if got := get(t, store, k.Meta.ID); got.Meta.Owner != project || got.Resolved != "sk-moving" {
		t.Fatalf("after the failed update: owner %+v, value %q", got.Meta.Owner, got.Resolved)
	}
}

func TestIntegration_OwnerChangeKeepsOAuthRefreshToken(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	store, sec := hostKeyStore(t, pool, masterKey)
	blob := `{"access_token":"at-1","refresh_token":"rt-1","expiry":"2099-01-01T00:00:00Z"}`
	k := storedKey(meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}, hostkey.ValueKindOAuth, blob)
	if err := store.Upsert(ctx, k); err != nil {
		t.Fatal(err)
	}
	// The oauth resolver is not registered here, so the row loads unresolved; the update still carries a resolved value as the API does.
	moved := get(t, store, k.Meta.ID)
	moved.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
	moved.Resolved = "at-1"
	if err := store.Upsert(ctx, moved); err != nil {
		t.Fatal(err)
	}
	raw, err := sec.Stored.Resolve(stored.WithScope(ctx, stored.ScopeSystem), secret.Ref{Kind: secret.KindStored, ID: k.Meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.RefreshToken != "rt-1" {
		t.Fatalf("stored blob after the owner change: %s (%v)", raw, err)
	}
}

func TestIntegration_TamperedValueLoadsUnresolved(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	store, sec := hostKeyStore(t, pool, masterKey)
	user := meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
	a := storedKey(user, hostkey.ValueKindStored, "sk-a")
	b := storedKey(user, hostkey.ValueKindStored, "sk-b")
	c := storedKey(meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}, hostkey.ValueKindStored, "sk-c")
	for _, k := range []*hostkey.HostKey{a, b, c} {
		if err := store.Upsert(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	unresolved := func(id string) {
		t.Helper()
		if k := get(t, store, id); k.Status.Unresolved == nil || k.Resolved != "" {
			t.Fatalf("%s resolved to %q, want unresolved", id, k.Resolved)
		}
	}
	copyValue := "UPDATE secret_values d SET ciphertext = s.ciphertext, scope = s.scope, dek_id = s.dek_id FROM secret_values s WHERE s.id = $1 AND d.id = $2"

	// Ciphertext copied to another row of the same scope: the row id is bound in.
	if _, err := pool.Exec(ctx, copyValue, a.Meta.ID, b.Meta.ID); err != nil {
		t.Fatal(err)
	}
	unresolved(b.Meta.ID)
	// Copied to a row whose owner is another scope.
	if _, err := pool.Exec(ctx, copyValue, a.Meta.ID, c.Meta.ID); err != nil {
		t.Fatal(err)
	}
	unresolved(c.Meta.ID)

	// The owner edited behind the store's back (a missed rescope): the owner's scope decides, so the value no longer opens.
	if _, err := pool.Exec(ctx, `UPDATE secrets SET metadata = jsonb_set(metadata, '{owner,id}', to_jsonb($2::text)) WHERE id = $1`, a.Meta.ID, meta.NewID()); err != nil {
		t.Fatal(err)
	}
	unresolved(a.Meta.ID)

	// An edited scope column (the trigger refuses it, so it is turned off here): readers that trust the column fail on the key's own scope.
	sys, err := sec.Stored.CreateScoped(ctx, stored.ScopeSystem, "auth-tokens-signing-key", []byte("seed"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{
		"ALTER TABLE secret_values DISABLE TRIGGER secret_values_check",
		"UPDATE secret_values SET scope = 'user:intruder' WHERE id = 'auth-tokens-signing-key'",
		"ALTER TABLE secret_values ENABLE TRIGGER secret_values_check",
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sec.Registry.Resolve(ctx, sys); err == nil || !strings.Contains(err.Error(), "another scope") {
		t.Fatalf("edited scope column: %v", err)
	}
}
