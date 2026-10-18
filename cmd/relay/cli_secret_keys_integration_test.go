//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret"
	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
)

// The key ring commands against Postgres with no server running: rotate, rewrap to an extra master key, destroy, and their refusals.
func TestIntegration_SecretKeyCommands(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)
	dsn := storagetest.DB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(oldKey))
	st, err := storage.Open(ctx, dsn, storage.WithMigrateOnBoot(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := stored.New(st.Pool(), [][]byte{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	for id, scope := range map[string]string{"u": "user:u1", "s": stored.ScopeSystem} {
		if _, err := b.CreateScoped(ctx, scope, id, []byte("v-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(out string) []stored.DataKeyStatus {
		t.Helper()
		var ks []stored.DataKeyStatus
		if err := json.Unmarshal([]byte(out), &ks); err != nil {
			t.Fatalf("output %q: %v", out, err)
		}
		return ks
	}

	if err := runSecret([]string{"rotate", "--scope", "user:nobody"}); !errors.Is(err, stored.ErrNoScopeKey) {
		t.Fatalf("rotate a scope with no key: %v", err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runSecret([]string{"rotate", "--scope", "user:u1"}) })
	if runErr != nil || len(keys(out)) != 1 || keys(out)[0].Scope != "user:u1" {
		t.Fatalf("rotate --scope: %v %s", runErr, out)
	}
	out = captureStdout(t, func() { runErr = runSecret([]string{"rotate", "--all"}) })
	if runErr != nil || len(keys(out)) != 2 {
		t.Fatalf("rotate --all: %v %s", runErr, out)
	}

	newKEK, err := local.New(newKey)
	if err != nil {
		t.Fatal(err)
	}
	to := stored.FormatKEKID(newKEK.ID())
	if err := runSecret([]string{"rewrap", "--to", to}); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("rewrap without the new key configured: %v", err)
	}
	t.Setenv("RELAY_MASTER_KEY_EXTRA", base64.StdEncoding.EncodeToString(newKey))
	out = captureStdout(t, func() { runErr = runSecret([]string{"rewrap", "--to", to}) })
	// Two first keys, one rotation of user:u1, then one per scope.
	var res stored.RewrapResult
	if runErr != nil || json.Unmarshal([]byte(out), &res) != nil || res.Rewrapped != 5 || res.Target != to {
		t.Fatalf("rewrap: %v %s", runErr, out)
	}

	// The rewriter has not run: the first user key still seals a value.
	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for _, k := range status.DataKeys {
		if k.Scope == "user:u1" && k.Rows == 1 && k.State == string(keyring.DecryptOnly) {
			first = k.ID
		}
	}
	if first == "" {
		t.Fatalf("no demoted user key holding the value: %+v", status.DataKeys)
	}
	if err := runSecret([]string{"destroy", first, "--force"}); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("destroy a key in use: %v", err)
	}
	if err := runSecret([]string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	if err := runSecret([]string{"destroy", first}); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy without force just after demotion: %v", err)
	}
	out = captureStdout(t, func() { runErr = runSecret([]string{"destroy", "--force", first}) })
	if runErr != nil || !strings.Contains(out, `"destroyed"`) {
		t.Fatalf("destroy --force: %v %s", runErr, out)
	}

	newOnly, err := stored.New(st.Pool(), [][]byte{newKey})
	if err != nil {
		t.Fatal(err)
	}
	for id := range map[string]bool{"u": true, "s": true} {
		got, err := newOnly.Resolve(ctx, secret.Ref{Kind: secret.KindStored, ID: id})
		if err != nil || string(got) != "v-"+id {
			t.Fatalf("%s with only the new master key: %q %v", id, got, err)
		}
	}
}

// The last-resort rollback from the docs: restore the copy migration 34 took, then roll the schema back. Values written after the upgrade are lost; the copied ones read with the old binary's decrypt.
func TestIntegration_SecretRestoreFromLegacyCopy(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x33}, 32)
	dsn := storagetest.EmptyDB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	if err := storage.MigrateTo(dsn, 33); err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := crypto.Encrypt(key, []byte("before"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "INSERT INTO secret_values (id, ciphertext, nonce, key_version) VALUES ('kept', $1, $2, 1)", ct, nonce); err != nil {
		t.Fatal(err)
	}
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	if err := runSecret([]string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(ctx, dsn, storage.WithMigrateOnBoot(false))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := stored.New(st.Pool(), [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateScoped(ctx, stored.ScopeSystem, "lost", []byte("after")); err != nil {
		t.Fatal(err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"SET LOCAL relay.secret_legacy_write = 'on'",
		"DELETE FROM secret_values",
		"INSERT INTO secret_values (id, ciphertext, nonce, key_version, created_at, updated_at) SELECT id, ciphertext, nonce, key_version, created_at, updated_at FROM secret_values_legacy",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runMigrate([]string{"down", "33"}); err != nil {
		t.Fatalf("relay migrate down 33 after the restore: %v", err)
	}
	var gotCT, gotNonce []byte
	if err := conn.QueryRow(ctx, "SELECT ciphertext, nonce FROM secret_values WHERE id = 'kept'").Scan(&gotCT, &gotNonce); err != nil {
		t.Fatal(err)
	}
	if got, err := crypto.Decrypt(key, gotCT, gotNonce); err != nil || string(got) != "before" {
		t.Fatalf("restored value: %q %v", got, err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM secret_values WHERE id = 'lost'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a value written after the upgrade survived the restore: %d %v", n, err)
	}
}
