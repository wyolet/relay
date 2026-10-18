//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/secret/stored"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret"
)

// legacyRow is a stored secret as a relay from before the key ring wrote it.
type legacyRow struct {
	id, scope string
	plain     []byte
	owner     *meta.Owner
	oauth     bool
	ct, nonce []byte
}

// magicLegacy encrypts plain under key like pkg/crypto does, choosing plaintext bytes so the ciphertext starts with the sealed format's magic and version, as about one legacy row in 2^32 does by chance.
func magicLegacy(t *testing.T, key []byte) (plain, ct, nonce []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce = bytes.Repeat([]byte{3}, gcm.NonceSize())
	stream := gcm.Seal(nil, nonce, make([]byte, 48), nil)[:48]
	want := append([]byte{0x00, 'W', 'S', 'K', 0x01}, bytes.Repeat([]byte{'m'}, 43)...)
	plain = make([]byte, 48)
	for i := range plain {
		plain[i] = want[i] ^ stream[i]
	}
	return plain, gcm.Seal(nil, nonce, plain, nil), nonce
}

// The one-time migration end to end: legacy rows under the master key, migration 34, `relay secret migrate`, then the rollback through `relay secret downgrade` and migration 34 down.
func TestIntegration_SecretMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x42}, 32)
	dsn := storagetest.EmptyDB(t)
	t.Setenv("RELAY_PG_DSN", dsn)
	t.Setenv("RELAY_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	if err := storage.MigrateTo(dsn, 33); err != nil {
		t.Fatalf("migrate down to 33 on an empty table: %v", err)
	}

	user, project := meta.NewID(), meta.NewID()
	magicPlain, magicCT, magicNonce := magicLegacy(t, key)
	rows := []*legacyRow{
		{id: meta.NewID(), scope: "user:" + user, plain: []byte("sk-user"), owner: &meta.Owner{Kind: meta.OwnerUser, ID: user}},
		{id: meta.NewID(), scope: "project:" + project, plain: []byte("sk-project"), owner: &meta.Owner{Kind: meta.OwnerProject, ID: project}},
		{id: meta.NewID(), scope: "system", plain: []byte("sk-system"), owner: &meta.Owner{Kind: meta.OwnerSystem}},
		{id: meta.NewID(), scope: "user:" + user, plain: []byte(`{"access_token":"at","refresh_token":"rt"}`), owner: &meta.Owner{Kind: meta.OwnerUser, ID: user}, oauth: true},
		{id: "auth-tokens-signing-key", scope: "system", plain: bytes.Repeat([]byte{7}, 32)},
		{id: "payload-logging:access-key", scope: "system", plain: []byte("DUMMYACCESSKEY01")},
		{id: "orphan-with-magic", scope: "system", plain: magicPlain, ct: magicCT, nonce: magicNonce},
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, r := range rows {
		if r.ct == nil {
			if r.ct, r.nonce, err = crypto.Encrypt(key, r.plain); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := conn.Exec(ctx, "INSERT INTO secret_values (id, ciphertext, nonce, key_version) VALUES ($1, $2, $3, 1)", r.id, r.ct, r.nonce); err != nil {
			t.Fatal(err)
		}
		if r.owner == nil {
			continue
		}
		md, _ := json.Marshal(map[string]any{"owner": r.owner})
		vf := map[string]string{"kind": "stored"}
		if r.oauth {
			vf = map[string]string{"kind": "oauth", "provider": "acme"}
		}
		spec, _ := json.Marshal(map[string]any{"hostId": meta.NewID(), "policyId": meta.NewID(), "valueFrom": vf})
		if _, err := conn.Exec(ctx, "INSERT INTO secrets (id, name, display_name, value_kind, metadata, spec) VALUES ($1, $2, '', 'stored', $3, $4)",
			r.id, "key-"+r.id[len(r.id)-8:], md, spec); err != nil {
			t.Fatal(err)
		}
	}

	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("migration 34 over legacy rows: %v", err)
	}
	var copied int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM secret_values_legacy").Scan(&copied); err != nil || copied != len(rows) {
		t.Fatalf("rollback copy: %d rows, %v", copied, err)
	}
	// The copy keeps its primary key: the fallback restore upserts on it.
	if _, err := conn.Exec(ctx, "INSERT INTO secret_values_legacy (id, ciphertext, nonce) VALUES ($1, $2, $3) ON CONFLICT (id) DO UPDATE SET ciphertext = EXCLUDED.ciphertext",
		rows[0].id, rows[0].ct, rows[0].nonce); err != nil {
		t.Fatalf("upsert on the rollback copy: %v", err)
	}

	if err := runSecret([]string{"migrate"}); err != nil {
		t.Fatalf("relay secret migrate: %v", err)
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
	for _, r := range rows {
		var scope string
		var dek *string
		if err := conn.QueryRow(ctx, "SELECT scope, dek_id::text FROM secret_values WHERE id = $1", r.id).Scan(&scope, &dek); err != nil {
			t.Fatal(err)
		}
		if scope != r.scope || dek == nil {
			t.Errorf("%s: scope %q dek %v, want scope %q under a data key", r.id, scope, dek, r.scope)
		}
		got, err := b.Resolve(stored.WithScope(ctx, r.scope), secret.Ref{Kind: secret.KindStored, ID: r.id})
		if err != nil || !bytes.Equal(got, r.plain) {
			t.Errorf("%s after migrate: %q, %v", r.id, got, err)
		}
	}
	status, err := b.Status(ctx)
	if err != nil || status.LegacyRows != 0 || len(status.DataKeys) != 3 {
		t.Fatalf("status after migrate: %+v %v (want 0 legacy rows, keys for user, project, system)", status, err)
	}

	// Rollback: downgrade rewrites every row to the legacy shape, then migration 34 down restores the previous schema.
	if err := runMigrate([]string{"down", "33"}); err == nil {
		t.Fatal("relay migrate down 33 ran while sealed rows remained")
	}
	if v := versionIn(t, dsn, "schema_migrations"); v != 34 {
		t.Fatalf("schema after the refused rollback = %d, want 34", v)
	}
	if err := runSecret([]string{"downgrade"}); err != nil {
		t.Fatalf("relay secret downgrade: %v", err)
	}
	if err := runMigrate([]string{"down", "33"}); err != nil {
		t.Fatalf("relay migrate down 33 after the downgrade: %v", err)
	}
	for _, r := range rows {
		var ct, nonce []byte
		if err := conn.QueryRow(ctx, "SELECT ciphertext, nonce FROM secret_values WHERE id = $1", r.id).Scan(&ct, &nonce); err != nil {
			t.Fatal(err)
		}
		if got, err := crypto.Decrypt(key, ct, nonce); err != nil || !bytes.Equal(got, r.plain) {
			t.Errorf("%s after the rollback: %q, %v", r.id, got, err)
		}
	}
	var leftovers int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'secret_values' AND column_name IN ('scope', 'dek_id')`).Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("columns left after migration 34 down: %d, %v", leftovers, err)
	}
}
