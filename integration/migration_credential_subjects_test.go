//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
)

// credentialSubjectsAt37 writes a host, one account with two keys, a policy and the bindings named, then goes back to 37, where policy bindings could still name a host key, and gives each binding the subjects subjects(keys, account) returns.
func credentialSubjectsAt37(t *testing.T, bindings []string, subjects func(keys [2]string, account string) map[string][]string) (dsn string, keys [2]string, acct string, ids map[string]string) {
	t.Helper()
	ctx := context.Background()
	dsn = storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := st.Pool()
	q := gen.New(db)
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	a := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: hst.Meta.ID}}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "key-cap", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	if err := account.NewStore(q).Upsert(ctx, a); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	ids = map[string]string{}
	for _, name := range bindings {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: policybinding.Spec{PolicyID: pol.Meta.ID},
		}
		if err := policybinding.NewStore(db).Upsert(ctx, b); err != nil {
			t.Fatalf("upsert binding %s: %v", name, err)
		}
		ids[name] = b.Meta.ID
	}
	for i := range keys {
		keys[i] = meta.NewID()
		if _, err := db.Exec(ctx, "INSERT INTO secrets (id, name, value_kind, value_from_env, metadata, spec) VALUES ($1, $2, 'env', 'ACME_API_KEY', '{\"owner\":{\"kind\":\"system\"}}', $3)",
			keys[i], "key-"+keys[i], `{"hostId":"`+hst.Meta.ID+`","accountId":"`+a.Meta.ID+`"}`); err != nil {
			t.Fatalf("insert key: %v", err)
		}
	}
	st.Close()

	if err := storagemod.MigrateTo(dsn, 37); err != nil {
		t.Fatalf("migrate down to 37: %v", err)
	}
	conn := testPool(t, dsn)
	for name, subs := range subjects(keys, a.Meta.ID) {
		for i, s := range subs {
			kind, id, _ := strings.Cut(s, ":")
			fk := map[string]string{"credential": "subject_credential_id", "account": "subject_account_id"}[kind]
			if _, err := conn.Exec(ctx, "INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, "+fk+", position) VALUES ($1, $2, $3, $3, $4)", ids[name], kind, id, i+1); err != nil {
				t.Fatalf("insert %s subject %s: %v", name, s, err)
			}
		}
	}
	return dsn, keys, a.Meta.ID, ids
}

// A binding naming host keys names their account after the upgrade, once however many of its keys it named.
func TestMigrateUpMovesCredentialSubjectsToTheirAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, _, acct, ids := credentialSubjectsAt37(t, []string{"on-keys"}, func(keys [2]string, _ string) map[string][]string {
		return map[string][]string{"on-keys": {"credential:" + keys[0], "credential:" + keys[1]}}
	})
	if err := storagemod.MigrateUp(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	rows, err := testPool(t, dsn).Query(ctx, "SELECT account_id FROM policy_binding_accounts WHERE binding_id = $1", ids["on-keys"])
	if err != nil {
		t.Fatalf("read bound accounts: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != acct {
		t.Fatalf("bound accounts = %v, want the one account %s", got, acct)
	}
}

// An account two bindings would hold after the upgrade, one through a key, stops it with both bindings named.
func TestMigrateUpRefusesAnAccountInTwoBindings(t *testing.T) {
	t.Parallel()
	dsn, _, acct, _ := credentialSubjectsAt37(t, []string{"on-key", "on-account"}, func(keys [2]string, account string) map[string][]string {
		return map[string][]string{"on-key": {"credential:" + keys[0]}, "on-account": {"account:" + account}}
	})
	err := storagemod.MigrateUp(dsn)
	if err == nil {
		t.Fatal("the upgrade went through with an account in two bindings")
	}
	for _, want := range []string{"account " + acct, "on-key", "on-account"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("upgrade error %q does not name %q", err, want)
		}
	}
}
