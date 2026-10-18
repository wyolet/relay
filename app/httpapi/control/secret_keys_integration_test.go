//go:build integration

package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
	"github.com/wyolet/relay/secret/kek/local"
)

var (
	ringKeyA = bytes.Repeat([]byte{0xa1}, 32)
	ringKeyB = bytes.Repeat([]byte{0xb2}, 32)
	ringKeyC = bytes.Repeat([]byte{0xc3}, 32)
)

func ringBackend(t *testing.T, pool *pgxpool.Pool, keys ...[]byte) *stored.Backend {
	t.Helper()
	b, err := stored.New(pool, keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kekID(t *testing.T, key []byte) string {
	t.Helper()
	k, err := local.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return stored.FormatKEKID(k.ID())
}

func resolveStored(t *testing.T, b *stored.Backend, id string) string {
	t.Helper()
	v, err := b.Resolve(context.Background(), secret.Ref{Kind: secret.KindStored, ID: id})
	if err != nil {
		t.Fatalf("resolve %s: %v", id, err)
	}
	return string(v)
}

func dekOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var dek string
	if err := pool.QueryRow(context.Background(), "SELECT dek_id::text FROM secret_values WHERE id = $1", id).Scan(&dek); err != nil {
		t.Fatal(err)
	}
	return dek
}

// assertNoWrappedBytes fails when a response body carries any data key's wrapped bytes, in hex or base64.
func assertNoWrappedBytes(t *testing.T, pool *pgxpool.Pool, bodies []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT wrapped FROM secret_data_keys WHERE wrapped IS NOT NULL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var w []byte
		if err := rows.Scan(&w); err != nil {
			t.Fatal(err)
		}
		n++
		// The header holds the KEK id, which responses do name; compare the nonce and ciphertext after it.
		tail := w[21:]
		for _, enc := range []string{hex.EncodeToString(tail), base64.StdEncoding.EncodeToString(tail)[:32], base64.RawURLEncoding.EncodeToString(tail)[:32]} {
			for _, b := range bodies {
				if strings.Contains(b, enc) {
					t.Fatalf("a /secret-keys response carries wrapped key bytes: %s", b)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no wrapped data keys to compare against")
	}
}

// Rotate through the API, then destroy the old key once the rewriter has emptied it.
func TestIntegration_SecretKeysRotateAndDestroy(t *testing.T) {
	pool := storagetest.Pool(t)
	b := ringBackend(t, pool, ringKeyA)
	if _, err := b.CreateScoped(context.Background(), "project:p1", "v", []byte("sk-project")); err != nil {
		t.Fatal(err)
	}
	old := dekOf(t, pool, "v")
	s := newSecretKeysHarness(t, b)
	var bodies []string

	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rotate", `{"scope":"project:p1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", w.Code, w.Body)
	}
	bodies = append(bodies, w.Body.String())
	var rotated struct{ Keys []stored.DataKeyStatus }
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || len(rotated.Keys) != 1 {
		t.Fatalf("rotate body %s: %v", w.Body, err)
	}
	newID := rotated.Keys[0].ID

	// Written before the rotation: still readable, here and on a process that never saw it, until the rewriter moves it.
	for name, p := range map[string]*stored.Backend{"same": b, "other": ringBackend(t, pool, ringKeyA)} {
		if got := resolveStored(t, p, "v"); got != "sk-project" {
			t.Fatalf("%s process after rotate: %q", name, got)
		}
	}
	if dekOf(t, pool, "v") != old {
		t.Fatal("value moved before the rewriter ran")
	}
	if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+old+"/destroy", `{"force":true}`); w.Code != http.StatusConflict {
		t.Fatalf("destroy a key in use = %d: %s", w.Code, w.Body)
	}
	if _, err := b.RewriteStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dekOf(t, pool, "v") != newID {
		t.Fatal("the rewriter did not move the value under the new key")
	}

	w = scopeReq(t, s.h, "root", http.MethodGet, "/secret-keys", "")
	bodies = append(bodies, w.Body.String())
	var st stored.KeyStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	for _, k := range st.DataKeys {
		if k.ID == old && (k.Rows != 0 || k.State != "decrypt") {
			t.Fatalf("old key after the rewrite: %+v", k)
		}
	}

	if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+newID+"/destroy", `{"force":true}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "primary") {
		t.Fatalf("destroy the Primary = %d: %s", w.Code, w.Body)
	}
	if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+old+"/destroy", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too recently") {
		t.Fatalf("destroy just after demotion = %d: %s", w.Code, w.Body)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE secret_data_keys SET demoted_at = NOW() - interval '25 hours' WHERE id = $1::uuid", old); err != nil {
		t.Fatal(err)
	}
	w = scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/by-id/"+old+"/destroy", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"destroyed"`) {
		t.Fatalf("destroy after the wait = %d: %s", w.Code, w.Body)
	}
	bodies = append(bodies, w.Body.String())
	if got := resolveStored(t, ringBackend(t, pool, ringKeyA), "v"); got != "sk-project" {
		t.Fatalf("value after its old key was destroyed: %q", got)
	}
	assertNoWrappedBytes(t, pool, bodies)

	evs := s.events()
	var actions []string
	for _, ev := range evs {
		actions = append(actions, ev.Action+"/"+ev.Outcome.Status)
	}
	if got := strings.Join(actions, ","); !strings.HasPrefix(got, "secret-keys.rotate/allowed,secret-keys.destroy/allowed") || strings.Count(got, "secret-keys.destroy") != 4 {
		t.Fatalf("audit rows = %s", got)
	}
}

// Rewrap through the API to a KEK held as an extra master key; a process holding only that key then reads every value.
func TestIntegration_SecretKeysRewrap(t *testing.T) {
	pool := storagetest.Pool(t)
	old := ringBackend(t, pool, ringKeyA)
	values := map[string]string{"u": "sk-user", "p": "sk-project", "s": "sk-system"}
	scopes := map[string]string{"u": "user:u1", "p": "project:p1", "s": stored.ScopeSystem}
	for id, v := range values {
		if _, err := old.CreateScoped(context.Background(), scopes[id], id, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	both := ringBackend(t, pool, ringKeyA, ringKeyB)
	s := newSecretKeysHarness(t, both)

	if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"`+kekID(t, ringKeyC)+`"}`); w.Code != http.StatusConflict {
		t.Fatalf("rewrap to a KEK this pod lacks = %d: %s", w.Code, w.Body)
	}
	if w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"not-a-kek"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("rewrap to a malformed id = %d: %s", w.Code, w.Body)
	}
	w := scopeReq(t, s.h, "root", http.MethodPost, "/secret-keys/rewrap", `{"to":"`+kekID(t, ringKeyB)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rewrap = %d: %s", w.Code, w.Body)
	}
	var res stored.RewrapResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Rewrapped != 3 || len(res.Skipped) != 0 || res.Target != kekID(t, ringKeyB) {
		t.Fatalf("rewrap body %s: %v", w.Body, err)
	}
	assertNoWrappedBytes(t, pool, []string{w.Body.String()})

	newOnly := ringBackend(t, pool, ringKeyB)
	for id, want := range values {
		if got := resolveStored(t, newOnly, id); got != want {
			t.Fatalf("%s on a process holding only the new key: %q", id, got)
		}
		if got := resolveStored(t, old, id); got != want {
			t.Fatalf("%s on a process still on the old env: %q", id, got)
		}
	}
	st, err := newOnly.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range st.DataKeys {
		if k.KEK != kekID(t, ringKeyB) || k.Locked {
			t.Fatalf("data key after the rewrap: %+v", k)
		}
	}
}
