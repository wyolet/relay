//go:build integration

package plan_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/engine"
)

// A user's key as the schema before 000036 stores it (principal in its column, an empty principal in the spec document, a policy id) authenticates as that user after the upgrade.
func TestIntegration_UserKeyAuthenticatesAfterUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	if err := storagemod.MigrateTo(dsn, 35); err != nil {
		t.Fatalf("migrate to 35: %v", err)
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(db.Close)

	userID, policyID, keyID := meta.NewID(), meta.NewID(), meta.NewID()
	sum := sha256.Sum256([]byte("rk_upgrade_user_key"))
	hash := hex.EncodeToString(sum[:])
	for _, stmt := range []struct {
		what, sql string
		args      []any
	}{
		{"user", `INSERT INTO users (id, username) VALUES ($1, 'upgrade-user')`, []any{userID}},
		{"policy", `INSERT INTO policies (id, name, metadata, spec) VALUES ($1, 'upgrade-policy', '{"owner":{"kind":"system"}}', '{}')`, []any{policyID}},
		{"key", `INSERT INTO relay_keys (id, name, key_hash, principal_user_id, metadata, spec) VALUES ($1, 'upgrade-key', $2, $3, $4, $5)`, []any{
			keyID, hash, userID,
			`{"owner":{"kind":"user","id":"` + userID + `"}}`,
			`{"enabled":true,"keyHash":"` + hash + `","policyId":"` + policyID + `","principal":{"kind":"","id":""}}`,
		}},
	} {
		if _, err := db.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert %s: %v", stmt.what, err)
		}
	}
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: db})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	v := loadPlans(t, stores)
	kp := engine.Get[plan.KeyPlanIndex](v, "").Lookup(hash, time.Now())
	if kp == nil {
		t.Fatalf("the key does not authenticate after the upgrade; issues: %+v", v.AllIssues())
	}
	if want := (key.Principal{Kind: key.PrincipalUser, ID: userID}); kp.Principal != want {
		t.Errorf("principal = %+v, want %+v", kp.Principal, want)
	}
	if kp.Caller == nil || kp.Caller.Refused != nil {
		t.Errorf("caller plan = %+v, want one that serves", kp.Caller)
	}
}
