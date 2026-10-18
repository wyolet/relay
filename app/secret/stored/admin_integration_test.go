//go:build integration

// admin_integration_test.go covers the key ring operations the control API and the CLI share: rotate, rewrap and destroy with their refusals, against a real Postgres. Run with: make test-integration.

package stored_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret/keyring"
)

var keyC = bytes.Repeat([]byte{0xc3}, 32)

func backdateDemotion(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE secret_data_keys SET demoted_at = NOW() - interval '25 hours' WHERE id = $1::uuid", id); err != nil {
		t.Fatal(err)
	}
}

func rowsUnder(t *testing.T, b *stored.Backend, id string) (int64, string) {
	t.Helper()
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range st.DataKeys {
		if k.ID == id {
			return k.Rows, k.State
		}
	}
	t.Fatalf("data key %s not in status", id)
	return 0, ""
}

func TestIntegration_RotateScopeAndAll(t *testing.T) {
	pool := storagetest.Pool(t)
	b := backend(t, pool, keyA)
	for id, scope := range map[string]string{"a": "user:u1", "b": "project:p1", "c": stored.ScopeSystem} {
		if _, err := b.CreateScoped(ctx, scope, id, []byte("v-"+id)); err != nil {
			t.Fatal(err)
		}
	}
	old := rowOf(t, pool, "a").dek

	if _, err := b.Rotate(ctx, "user:nobody"); !errors.Is(err, stored.ErrNoScopeKey) {
		t.Fatalf("rotate a scope with no key: %v", err)
	}
	k, err := b.Rotate(ctx, "user:u1")
	if err != nil {
		t.Fatal(err)
	}
	if k.Scope != "user:u1" || k.State != string(keyring.Primary) || k.ID == old || k.Rows != 0 {
		t.Fatalf("new key = %+v", k)
	}
	// The value written before stays readable under the demoted key until the rewriter moves it.
	if got := resolve(t, ctx, b, "a"); got != "v-a" || rowOf(t, pool, "a").dek != old {
		t.Fatalf("value after rotate: %q under %s", got, rowOf(t, pool, "a").dek)
	}
	if n, state := rowsUnder(t, b, old); n != 1 || state != string(keyring.DecryptOnly) {
		t.Fatalf("old key: %d rows, %s", n, state)
	}
	if _, err := b.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}
	if rowOf(t, pool, "a").dek != k.ID {
		t.Fatal("the rewriter left the value under the old key")
	}

	all, err := b.RotateAll(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("rotate all: %+v %v, want one key per scope", all, err)
	}
	for i, want := range []string{"project:p1", stored.ScopeSystem, "user:u1"} {
		if all[i].Scope != want {
			t.Fatalf("rotate all scope %d = %q, want %q", i, all[i].Scope, want)
		}
	}
}

func TestIntegration_RewrapRefusalsAndSkippedKeys(t *testing.T) {
	pool := storagetest.Pool(t)
	insertLegacy(t, pool, "old", []byte("legacy"))
	a := backend(t, pool, keyA)
	if _, err := a.CreateScoped(ctx, "user:u1", "v", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	both := backend(t, pool, keyA, keyB)
	st, err := both.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kekA, kekB := st.HeldKEKs[0], st.HeldKEKs[1]

	if _, err := a.Rewrap(ctx, kekB); !errors.Is(err, keyring.ErrTargetKEKUnavailable) {
		t.Fatalf("rewrap to a KEK this process lacks: %v", err)
	}
	if _, err := both.Rewrap(ctx, kekB); !errors.Is(err, stored.ErrLegacyRows) {
		t.Fatalf("rewrap with a legacy row left: %v", err)
	}
	if st, _ := both.Status(ctx); st.TargetKEK != kekA {
		t.Fatalf("a refused rewrap moved the target to %s", st.TargetKEK)
	}
	if _, err := both.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}

	// A process holding the target but not the KEK the data keys are under moves the target and names what it skipped.
	other := backend(t, pool, keyB, keyC)
	res, err := other.Rewrap(ctx, kekB)
	if err != nil {
		t.Fatalf("rewrap with locked keys: %v", err)
	}
	if res.Target != kekB || res.Rewrapped != 0 || len(res.Skipped) != 2 {
		t.Fatalf("rewrap with locked keys: %+v, want the user and system keys skipped", res)
	}
	res, err = both.Rewrap(ctx, kekB)
	if err != nil || res.Rewrapped != 2 || len(res.Skipped) != 0 {
		t.Fatalf("rewrap where both KEKs are held: %+v %v", res, err)
	}
	newOnly := backend(t, pool, keyB)
	for id, want := range map[string]string{"v": "secret", "old": "legacy"} {
		if got := resolve(t, ctx, newOnly, id); got != want {
			t.Fatalf("%s on a process holding only the new key: %q", id, got)
		}
	}
}

func TestIntegration_DestroyRefusals(t *testing.T) {
	pool := storagetest.Pool(t)
	b := backend(t, pool, keyA)
	if _, err := b.CreateScoped(ctx, "user:u1", "v", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	old := rowOf(t, pool, "v").dek
	if _, err := b.Destroy(ctx, old, true); !errors.Is(err, keyring.ErrIsPrimary) {
		t.Fatalf("destroy the Primary: %v", err)
	}
	if _, err := b.Rotate(ctx, "user:u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Destroy(ctx, old, false); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy just after demotion: %v", err)
	}
	backdateDemotion(t, pool, old)
	if _, err := b.Destroy(ctx, old, false); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("destroy while a value uses the key: %v", err)
	}
	if _, err := b.Destroy(ctx, old, true); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("force destroy while a value uses the key: %v", err)
	}
	if _, err := b.Destroy(ctx, "0192f000-0000-7000-8000-000000000009", false); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("destroy an unknown key: %v", err)
	}
	if _, err := b.Destroy(ctx, "nope", false); !errors.Is(err, stored.ErrBadKeyID) {
		t.Fatalf("destroy a malformed id: %v", err)
	}
	if _, err := b.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}
	k, err := b.Destroy(ctx, old, false)
	if err != nil || k.ID != old || k.State != string(keyring.Destroyed) {
		t.Fatalf("destroy after the rewriter and the wait: %+v %v", k, err)
	}
	if got := resolve(t, ctx, b, "v"); got != "secret" {
		t.Fatalf("value after destroying its old key: %q", got)
	}

	// Force skips only the wait: a key demoted a moment ago and emptied by the rewriter goes at once.
	second := rowOf(t, pool, "v").dek
	if _, err := b.Rotate(ctx, "user:u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RewriteStale(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Destroy(ctx, second, false); !errors.Is(err, keyring.ErrTooRecent) {
		t.Fatalf("destroy without force just after demotion: %v", err)
	}
	if _, err := b.Destroy(ctx, second, true); err != nil {
		t.Fatalf("force destroy of an emptied key: %v", err)
	}
}
