//go:build integration

// stored_integration_test.go covers the stored backend against a real Postgres: the secret_values trigger, two processes sharing one database, a process holding the wrong master key, and a KEK change. Run with: make test-integration.

package stored_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret"
	"github.com/wyolet/relay/secret/keyring"
)

var (
	ctx    = context.Background()
	keyA   = bytes.Repeat([]byte{0xa1}, 32)
	keyB   = bytes.Repeat([]byte{0xb2}, 32)
	keyBad = bytes.Repeat([]byte{0xee}, 32)
)

func storedRef(id string) secret.Ref { return secret.Ref{Kind: secret.KindStored, ID: id} }

func valueAAD(id string) []byte { return []byte("relay/secret_values/" + id) }

func backend(t *testing.T, pool *pgxpool.Pool, keys ...[]byte) *stored.Backend {
	t.Helper()
	b, err := stored.New(pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ringOf(t *testing.T, b *stored.Backend) *keyring.Ring {
	t.Helper()
	r, err := b.Ring(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type valueRow struct {
	ct    []byte
	scope string
	dek   string
}

func rowOf(t *testing.T, pool *pgxpool.Pool, id string) valueRow {
	t.Helper()
	var r valueRow
	err := pool.QueryRow(ctx, "SELECT ciphertext, COALESCE(scope, ''), COALESCE(dek_id::text, '') FROM secret_values WHERE id = $1", id).
		Scan(&r.ct, &r.scope, &r.dek)
	if err != nil {
		t.Fatalf("read secret_values %s: %v", id, err)
	}
	return r
}

// keyID parses a key id in UUID or plain hex form.
func keyID(t *testing.T, s string) [16]byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		t.Fatalf("key id %q: %v", s, err)
	}
	return [16]byte(b)
}

func resolve(t *testing.T, c context.Context, b *stored.Backend, id string) string {
	t.Helper()
	v, err := b.Resolve(c, storedRef(id))
	if err != nil {
		t.Fatalf("resolve %s: %v", id, err)
	}
	return string(v)
}

func countKeys(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM secret_data_keys").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegration_TriggerChecksRows(t *testing.T) {
	pool := storagetest.Pool(t)
	b := backend(t, pool, keyA)
	if _, err := b.CreateScoped(ctx, stored.ScopeSystem, "sys", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateScoped(ctx, "user:u1", "usr", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := crypto.Encrypt(keyA, []byte("legacy"))
	if err != nil {
		t.Fatal(err)
	}

	// Legacy bytes behind a dek_id would let Destroy's in-use count miss data that still needs the master key.
	_, err = pool.Exec(ctx, "UPDATE secret_values SET ciphertext = $2, nonce = $3 WHERE id = $1", "sys", ct, nonce)
	if err == nil || !strings.Contains(err.Error(), "WSK02") {
		t.Fatalf("legacy bytes with a dek_id: %v", err)
	}
	_, err = pool.Exec(ctx, "UPDATE secret_values SET dek_id = $2::uuid WHERE id = $1", "sys", rowOf(t, pool, "usr").dek)
	if err == nil || !strings.Contains(err.Error(), "WSK02") {
		t.Fatalf("dek_id other than the header's: %v", err)
	}
	_, err = pool.Exec(ctx, "UPDATE secret_values SET scope = 'user:u2' WHERE id = $1", "usr")
	if err == nil || !strings.Contains(err.Error(), "WSK01") {
		t.Fatalf("scope other than the key's: %v", err)
	}

	// A binary from before the key ring writes the legacy shape: refused unless the downgrade setting is on.
	insert := "INSERT INTO secret_values (id, ciphertext, nonce, key_version) VALUES ('old', $1, $2, 1)"
	if _, err = pool.Exec(ctx, insert, ct, nonce); err == nil || !strings.Contains(err.Error(), "WSK03") {
		t.Fatalf("legacy write without the setting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL relay.secret_legacy_write = 'on'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, insert, ct, nonce); err != nil {
		t.Fatalf("legacy write with the setting: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, ctx, b, "old"); got != "legacy" {
		t.Fatalf("legacy row = %q", got)
	}
}

func TestIntegration_TwoProcesses(t *testing.T) {
	pool := storagetest.Pool(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN catalog_events"); err != nil {
		t.Fatal(err)
	}

	a, b := backend(t, pool, keyA), backend(t, pool, keyA)
	if _, err := a.CreateScoped(ctx, stored.ScopeSystem, "x", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, ctx, b, "x"); got != "one" {
		t.Fatalf("B reads A's first write: %q", got)
	}
	x := rowOf(t, pool, "x")
	waitNotify(t, conn, "secretkey:upsert:target")
	waitNotify(t, conn, "secretkey:upsert:"+x.dek)

	nk, err := ringOf(t, a).Rotate(ctx, stored.ScopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	second := keyring.FormatID(nk.ID)
	waitNotify(t, conn, "secretkey:upsert:"+second)

	// B missed the rotation: the trigger refuses its write under the demoted key, and the retry lands under the new Primary.
	if _, err := b.CreateScoped(ctx, stored.ScopeSystem, "y", []byte("two")); err != nil {
		t.Fatalf("B write after a missed rotation: %v", err)
	}
	if got := rowOf(t, pool, "y").dek; got != second {
		t.Fatalf("B wrote under %s, want the new Primary %s", got, second)
	}

	// A rotates again and writes; B decrypts it without a notification, then is told.
	third, err := ringOf(t, a).Rotate(ctx, stored.ScopeSystem)
	if err != nil {
		t.Fatal(err)
	}
	thirdID := keyring.FormatID(third.ID)
	if _, err := a.CreateScoped(ctx, stored.ScopeSystem, "z", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, ctx, b, "z"); got != "three" {
		t.Fatalf("B reads a row under a key it never heard of: %q", got)
	}
	if err := b.KeyChanged(ctx, thirdID); err != nil {
		t.Fatal(err)
	}

	// The first key still seals x: destroy is refused, forced or not, until the rewriter moves it.
	if err := ringOf(t, a).ForceDestroy(ctx, keyID(t, x.dek)); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("force destroy of a key in use: %v", err)
	}
	res, err := a.RewriteStale(ctx)
	if err != nil || res.Rewritten != 2 || res.Failed != 0 {
		t.Fatalf("rewrite: %+v %v, want x and y moved", res, err)
	}
	for id, want := range map[string]string{"x": "one", "y": "two", "z": "three"} {
		if got, r := resolve(t, ctx, b, id), rowOf(t, pool, id); got != want || r.dek != thirdID {
			t.Fatalf("%s after rewrite: %q under %s", id, got, r.dek)
		}
	}
	if err := ringOf(t, a).ForceDestroy(ctx, keyID(t, x.dek)); err != nil {
		t.Fatalf("force destroy once unused: %v", err)
	}
	// B still holds the destroyed key unwrapped; after the notification it refuses it, as does a fresh process.
	if err := b.KeyChanged(ctx, x.dek); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*keyring.Ring{"notified": ringOf(t, b), "fresh": ringOf(t, backend(t, pool, keyA))} {
		if _, _, err := r.Decrypt(ctx, stored.ScopeSystem, x.ct, valueAAD("x")); !errors.Is(err, keyring.ErrDestroyed) {
			t.Fatalf("%s process opening a value under the destroyed key: %v", name, err)
		}
	}
	// A write that names the destroyed key is refused outright.
	_, err = pool.Exec(ctx, "UPDATE secret_values SET ciphertext = $2, dek_id = $3::uuid, scope = 'system' WHERE id = $1", "x", x.ct, x.dek)
	if err == nil || !strings.Contains(err.Error(), "WSK01") {
		t.Fatalf("write under a destroyed key: %v", err)
	}
}

// A process whose RELAY_MASTER_KEY is wrong keeps serving: the rows it cannot read fail to resolve, and it creates no data key under its own KEK.
func TestIntegration_WrongMasterKey(t *testing.T) {
	pool := storagetest.Pool(t)
	good := backend(t, pool, keyA)
	if _, err := good.CreateScoped(ctx, stored.ScopeSystem, "v", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	before := countKeys(t, pool)

	bad := backend(t, pool, keyBad)
	if _, err := bad.Resolve(ctx, storedRef("v")); err == nil {
		t.Fatal("a process with the wrong master key resolved the value")
	}
	if _, err := bad.CreateScoped(ctx, "user:new", "w", []byte("x")); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("create with the wrong master key: %v", err)
	}
	if _, err := bad.CreateScoped(ctx, stored.ScopeSystem, "w", []byte("x")); !errors.Is(err, keyring.ErrKEKUnavailable) {
		t.Fatalf("write under a Primary this process cannot unwrap: %v", err)
	}
	if got := countKeys(t, pool); got != before {
		t.Fatalf("data keys: %d, want %d", got, before)
	}
	st, err := bad.Status(ctx)
	if err != nil || len(st.DataKeys) != before || !st.DataKeys[0].Locked {
		t.Fatalf("status: %+v %v", st, err)
	}
}

// The local-KEK change runbook: a pod still on the old env keeps reading after the rewrap, and a pod with only the new key reads everything.
func TestIntegration_KEKChange(t *testing.T) {
	pool := storagetest.Pool(t)
	old := backend(t, pool, keyA)
	if _, err := old.CreateScoped(ctx, "user:u", "v", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	both := backend(t, pool, keyA, keyB)
	newOnly := backend(t, pool, keyB)
	st, err := both.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ringOf(t, both).Rewrap(ctx, keyID(t, st.HeldKEKs[1])); err != nil || n != 1 {
		t.Fatalf("rewrap: %d %v", n, err)
	}
	for name, b := range map[string]*stored.Backend{"old": old, "both": both, "new": newOnly} {
		if got := resolve(t, ctx, b, "v"); got != "secret" {
			t.Fatalf("%s pod: %q", name, got)
		}
	}
	if _, err := newOnly.CreateScoped(ctx, "user:other", "w", []byte("x")); err != nil {
		t.Fatalf("new pod creates a key under the new target: %v", err)
	}
	if _, err := old.CreateScoped(ctx, "user:third", "w2", []byte("x")); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("old pod creating a key after the target moved: %v", err)
	}
}

func waitNotify(t *testing.T, conn *pgxpool.Conn, payload string) {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		n, err := conn.Conn().WaitForNotification(c)
		if err != nil {
			t.Fatalf("waiting for %q: %v", payload, err)
		}
		if n.Payload == payload {
			return
		}
	}
}
