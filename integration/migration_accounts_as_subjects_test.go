//go:build integration

package integration_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

// subjectsRoundTrip is a binding per kind of bound account, written at head; bindings holds each binding's id by name, and the rest the rows the bindings and keys point at.
type subjectsRoundTrip struct {
	dsn                string
	team               *team.Team
	project            *project.Project
	sa                 *serviceaccount.ServiceAccount
	alice              *user.User
	provider           *account.Account
	hostKey            *hostkey.HostKey
	memberPolicy, wide *policy.Policy
	bindings           map[string]string
	aliceKey, saKey    string
}

func writeSubjectsAtHead(t *testing.T) *subjectsRoundTrip {
	t.Helper()
	ctx := context.Background()
	dsn := storagetest.EmptyDB(t)
	st, err := storagemod.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	db := st.Pool()
	q := gen.New(db)
	system := meta.Owner{Kind: meta.OwnerSystem}
	f := &subjectsRoundTrip{dsn: dsn, bindings: map[string]string{}}
	f.team = &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: system}}
	f.project = &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "search", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}}, Spec: project.Spec{TeamID: f.team.Meta.ID}}
	f.sa = &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: f.project.Meta.ID}}
	f.sa.StampOwner()
	f.alice = &user.User{ID: meta.NewID(), Username: "alice"}
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	f.provider = &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-org", Owner: system}, Spec: account.Spec{HostID: hst.Meta.ID}}
	f.hostKey = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-key", Owner: system},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: f.provider.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
	}
	accounts := account.NewStore(q)
	for _, step := range []struct {
		name  string
		write func() error
	}{
		{"team", func() error { return team.NewStore(db).Upsert(ctx, f.team) }},
		{"project", func() error { return project.NewStore(db).Upsert(ctx, f.project) }},
		{"service account", func() error { return serviceaccount.NewStore(db).Upsert(ctx, f.sa) }},
		{"user", func() error { return user.NewStore(db).Upsert(ctx, f.alice) }},
		{"host", func() error { return host.NewStore(q).Upsert(ctx, hst) }},
		{"provider account", func() error { return accounts.Upsert(ctx, f.provider) }},
		{"host key", func() error { return hostkey.NewStore(db, nil, nil).Upsert(ctx, f.hostKey) }},
	} {
		if err := step.write(); err != nil {
			t.Fatalf("upsert %s: %v", step.name, err)
		}
	}
	managed := func(want *account.Account) string {
		id, err := accounts.EnsureManaged(ctx, want)
		if err != nil {
			t.Fatalf("ensure %s: %v", want.Meta.Name, err)
		}
		return id
	}
	inProject := meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID}
	member := account.Member(f.alice.ID, f.alice.Username, inProject, f.project.Meta.Name, "")
	member.Spec.PayloadLogging = account.PayloadLoggingOn
	lanes := map[string]struct {
		owner     meta.Owner
		accountID string
	}{
		"on-team":     {system, managed(account.ForTeam(f.team.Meta.ID, f.team.Meta.Name, ""))},
		"on-project":  {inProject, managed(account.ForProject(f.project.Meta.ID, f.project.Meta.Name, ""))},
		"on-indexer":  {inProject, managed(account.ForServiceAccount(f.sa.Meta.ID, f.sa.Meta.Name, "", f.project.Meta.ID))},
		"on-alice":    {inProject, managed(member)},
		"on-acme-org": {system, f.provider.Meta.ID},
	}
	personal := managed(account.Personal(f.alice.ID, f.alice.Username))
	policies := policy.NewStore(db)
	for name, lane := range lanes {
		pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-policy", Owner: system}}
		if err := policies.Upsert(ctx, pol); err != nil {
			t.Fatalf("upsert policy %s: %v", pol.Meta.Name, err)
		}
		switch name {
		case "on-alice":
			f.memberPolicy = pol
		case "on-acme-org":
			f.wide = pol
		}
		b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: lane.owner}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{lane.accountID}}}
		if err := policybinding.NewStore(db).Upsert(ctx, b); err != nil {
			t.Fatalf("upsert binding %s: %v", name, err)
		}
		f.bindings[name] = b.Meta.ID
	}
	keys := key.NewStore(q)
	for _, k := range []struct {
		id        *string
		principal key.Principal
		lane      string
	}{
		{&f.aliceKey, key.Principal{Kind: key.PrincipalUser, ID: f.alice.ID}, personal},
		{&f.saKey, key.Principal{Kind: key.PrincipalServiceAccount, ID: f.sa.Meta.ID}, lanes["on-indexer"].accountID},
	} {
		row := &key.Key{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "key-" + string(k.principal.Kind), Owner: system},
			Spec: key.Spec{Principal: k.principal, AccountID: k.lane, KeyHash: strings.Repeat("a", 63) + string(k.principal.Kind[0])},
		}
		if err := keys.Upsert(ctx, row); err != nil {
			t.Fatalf("upsert key: %v", err)
		}
		*k.id = row.Meta.ID
	}
	return f
}

// Rolling back below 000039 turns each bound caller account back into the subject it stands for and keeps a provider account an account subject; upgrading again binds the same places, folds what the older schema still carried (a group subject, a directly linked credential, an @host model, a policy's payload-logging flag) into the account form, and puts every key back in its principal's account.
func TestMigrateDownAndUpAcrossAccountsAsSubjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := writeSubjectsAtHead(t)

	if err := storagemod.MigrateTo(f.dsn, 38); err != nil {
		t.Fatalf("migrate down to 38: %v", err)
	}
	conn := testPool(t, f.dsn)
	wantSubjects := map[string]string{
		"on-team": "team:" + f.team.Meta.ID, "on-project": "project:" + f.project.Meta.ID, "on-indexer": "serviceaccount:" + f.sa.Meta.ID,
		"on-alice": "user:" + f.alice.ID, "on-acme-org": "account:" + f.provider.Meta.ID,
	}
	for name, want := range wantSubjects {
		var got string
		if err := conn.QueryRow(ctx, "SELECT kind || ':' || subject_id FROM policy_binding_subjects WHERE binding_id = $1", f.bindings[name]).Scan(&got); err != nil || got != want {
			t.Errorf("binding %s at 38 = %q (err %v), want %q", name, got, err, want)
		}
	}
	var logged bool
	if err := conn.QueryRow(ctx, "SELECT COALESCE((spec->>'payloadLoggingEnabled')::boolean, false) FROM policies WHERE id = $1", f.memberPolicy.Meta.ID).Scan(&logged); err != nil || !logged {
		t.Errorf("the capturing account's policy at 38: payloadLoggingEnabled=%v (err %v), want true", logged, err)
	}

	for what, stmt := range map[string]struct {
		sql  string
		args []any
	}{
		"group subject":      {"INSERT INTO policy_binding_subjects (binding_id, kind, subject_name, position, owner_scope) VALUES ($1, 'group', 'system:authenticated', 5, '')", []any{f.bindings["on-team"]}},
		"linked credential":  {"INSERT INTO policy_credentials (policy_id, host_key_id, position) VALUES ($1, $2, 0)", []any{f.wide.Meta.ID, f.hostKey.Meta.ID}},
		"models with a host": {`UPDATE policies SET models = '["acme/m@acme", "acme@acme", "acme/m"]', spec = spec || '{"includeDeprecated": true}' WHERE id = $1`, []any{f.wide.Meta.ID}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("insert %s at 38: %v", what, err)
		}
	}

	if err := storagemod.MigrateUp(f.dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	st, err := storagemod.Open(ctx, f.dsn)
	if err != nil {
		t.Fatalf("open at head: %v", err)
	}
	defer st.Close()
	q := gen.New(st.Pool())
	all, err := account.NewStore(q).List(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	byID := map[string]*account.Account{}
	byPlace := map[string]*account.Account{}
	for _, a := range all {
		byID[a.Meta.ID] = a
		byPlace[a.ManagedKey()] = a
	}
	wantPlaces := map[string]string{
		"on-team": "team:" + f.team.Meta.ID, "on-project": "project:" + f.project.Meta.ID, "on-indexer": "service:" + f.sa.Meta.ID,
		"on-alice": "member:project:" + f.project.Meta.ID + ":" + f.alice.ID, "on-acme-org": "",
	}
	bindings := policybinding.NewStore(st.Pool())
	for name, place := range wantPlaces {
		b, err := bindings.Get(ctx, f.bindings[name])
		if err != nil || b == nil || len(b.Spec.AccountIDs) != 1 {
			t.Fatalf("binding %s at head = %+v (err %v), want one account", name, b, err)
		}
		a := byID[b.Spec.AccountIDs[0]]
		if a == nil || a.ManagedKey() != place || (place == "" && a.Meta.ID != f.provider.Meta.ID) {
			t.Errorf("binding %s binds %+v, want the account at %q", name, a, place)
		}
	}
	if m := byPlace["member:project:"+f.project.Meta.ID+":"+f.alice.ID]; m == nil || m.Spec.PayloadLogging != account.PayloadLoggingOn {
		t.Errorf("alice's member account = %+v, want payload logging on from its policy", m)
	}

	wide, err := policy.NewStore(st.Pool()).Get(ctx, f.wide.Meta.ID)
	if err != nil || wide == nil {
		t.Fatalf("read the provider account's policy: %v", err)
	}
	if !slices.Equal(wide.Spec.Models, []string{"acme/m", "acme"}) {
		t.Errorf("models = %v, want the host suffixes stripped and deduplicated", wide.Spec.Models)
	}
	if len(wide.Spec.PoolIDs) != 1 {
		t.Fatalf("pools = %v, want the linked credential as a pool of one", wide.Spec.PoolIDs)
	}
	pl, err := pool.NewStore(st.Pool()).Get(ctx, wide.Spec.PoolIDs[0])
	if err != nil || pl == nil || !slices.Equal(pl.Spec.CredentialIDs, []string{f.hostKey.Meta.ID}) {
		t.Errorf("the credential's pool = %+v (err %v), want it holding the host key", pl, err)
	}

	keys := key.NewStore(q)
	for id, place := range map[string]string{f.aliceKey: "personal:" + f.alice.ID, f.saKey: "service:" + f.sa.Meta.ID} {
		k, err := keys.Get(ctx, id)
		if err != nil || k == nil || byID[k.Spec.AccountID] == nil || byID[k.Spec.AccountID].ManagedKey() != place {
			t.Errorf("key %s runs in %+v (err %v), want the account at %s", id, k, err, place)
		}
	}
}
