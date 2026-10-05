//go:build integration

// upgrade_test.go loads rows the way the pre-tenancy schema (migration 24)
// wrote them, runs the tenancy migrations over them, and checks the upgraded
// relay still serves them as before.

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/ids"
)

// preTenancyVersion is the last schema version before teams and projects.
const preTenancyVersion = 24

// upgradeDB is a database of its own migrated to the pre-tenancy schema, with
// stores bound to it.
type upgradeDB struct {
	dsn    string
	pool   *pgxpool.Pool
	m      *migrate.Migrate
	cat    *appcatalog.Catalog
	stores *appcatalog.Stores
}

func newUpgradeDB(t *testing.T) *upgradeDB {
	t.Helper()
	dsn := storagetest.EmptyDB(t)
	m := migrator(t, dsn)
	if err := m.Migrate(preTenancyVersion); err != nil {
		t.Fatalf("migrate to %d: %v", preTenancyVersion, err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// The stores seeding the old rows are today's, and every catalog upsert
	// names resource_version; the later migration adds it IF NOT EXISTS.
	for _, table := range []string{"providers", "hosts", "models", "secrets", "rate_limits", "policies", "pricings", "host_bindings", "relay_keys"} {
		if _, err := pool.Exec(ctx, "ALTER TABLE "+table+" ADD COLUMN resource_version BIGINT NOT NULL DEFAULT 1"); err != nil {
			t.Fatalf("add resource_version to %s: %v", table, err)
		}
	}
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	return &upgradeDB{dsn: dsn, pool: pool, m: m, cat: cat, stores: stores}
}

func (u *upgradeDB) upgrade(t *testing.T) {
	t.Helper()
	if err := u.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up: %v", err)
	}
}

// hydrate loads the upgraded rows with the policy-less flow switched on.
func (u *upgradeDB) hydrate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(settings.Inference{AllowMissingPolicy: true})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if _, err := u.stores.Settings.Upsert(ctx, settings.SectionInference, raw); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if _, err := u.cat.Hydrate(ctx, u.stores, appcatalog.BootstrapOptions{Pool: u.pool}); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
}

// seedRoute writes one routable model behind one host key, the way the
// pre-tenancy control API wrote them, and returns the model's name.
func (u *upgradeDB) seedRoute(t *testing.T, hostKeyOwner meta.Owner) string {
	t.Helper()
	ctx := context.Background()
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "acme", Owner: system}}
	mustUpsert(t, u.stores.Provider.Upsert(ctx, prov), "provider")
	h := &host.Host{
		Meta: meta.Metadata{ID: ids.New(), Name: "acme-api", Owner: system},
		Spec: host.Spec{BaseURL: "https://acme.example.com"},
	}
	mustUpsert(t, u.stores.Host.Upsert(ctx, h), "host")
	tier := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "acme-api-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}}}
	mustUpsert(t, u.stores.Policy.Upsert(ctx, tier), "tier policy")
	t.Setenv("UPGRADE_TEST_HOST_KEY", "sk-upstream")
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: ids.New(), Name: "acme-key", Owner: hostKeyOwner},
		Spec: hostkey.Spec{HostID: h.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "UPGRADE_TEST_HOST_KEY"}},
	}
	mustUpsert(t, u.stores.HostKey.Upsert(ctx, hk), "host key")
	md := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "acme-chat", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "acme-chat"}}, Pointer: "acme-chat"},
	}
	mustUpsert(t, u.stores.Model.Upsert(ctx, md), "model")
	b := &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "acme-chat-on-acme-api", Owner: system},
		Spec: binding.Spec{ModelID: md.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI},
	}
	mustUpsert(t, u.stores.Binding.Upsert(ctx, b), "binding")
	return "acme-chat"
}

// insertPreTenancyKey writes a relay_keys row as the pre-tenancy control API
// did for the admin token: owner kind user with no id, and no policy.
func (u *upgradeDB) insertPreTenancyKey(t *testing.T, plaintext string) {
	t.Helper()
	hash := sha256Hex(plaintext)
	if _, err := u.pool.Exec(context.Background(),
		`INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec)
		 VALUES ($1, 'ci-bot', 'CI bot', $2,
		         '{"owner":{"kind":"user"}}'::jsonb,
		         jsonb_build_object('keyHash', $2::text, 'prefix', 'rk_ci'))`,
		ids.New(), hash); err != nil {
		t.Fatalf("insert pre-tenancy key: %v", err)
	}
}

// authenticate runs the inference credential middleware and returns the
// status and the principal it resolved.
func authenticate(t *testing.T, cat *appcatalog.Catalog, plaintext string) (int, *inference.Principal) {
	t.Helper()
	var seen *inference.Principal
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = inference.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := inference.ClassifyMiddleware()(inference.PrincipalMiddleware(cat, nil)(inner))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, seen
}

// A key with no policy served the policy-less flow before the upgrade. The
// migration parks it in the legacy project, and it must keep serving exactly
// as before: under the inference setting in single mode, refused under rbac.
func TestUpgradedKeyWithoutPolicyKeepsPolicylessAccess(t *testing.T) {
	u := newUpgradeDB(t)
	modelName := u.seedRoute(t, meta.Owner{Kind: meta.OwnerSystem})
	u.insertPreTenancyKey(t, "rk_ci_plaintext")
	u.upgrade(t)
	u.hydrate(t)

	code, p := authenticate(t, u.cat, "rk_ci_plaintext")
	if code != http.StatusOK {
		t.Fatalf("upgraded key answered %d, want it authenticated", code)
	}
	if p.Policy != nil {
		t.Fatalf("upgraded key resolved policy %q, want none", p.Policy.Meta.Name)
	}
	req := routing.Request{ModelName: modelName, UserID: p.UserID, Snapshot: u.cat.Current()}
	if _, err := routing.New(u.cat).Resolve(req); err != nil {
		t.Fatalf("single mode: the upgraded key no longer routes: %v", err)
	}
	if _, err := routing.New(u.cat, routing.RequirePolicy()).Resolve(req); !errors.Is(err, routing.ErrPolicyless) {
		t.Fatalf("rbac mode: err = %v, want the policy-less refusal", err)
	}
}

// The older binary reads a key's policy from spec.policyId only. A key that
// resolved its policy through its service account or a policy binding must
// carry that same policy after a rollback, not turn policy-less.
func TestRollbackWritesTheResolvedPolicyOntoEachKey(t *testing.T) {
	dsn := storagetest.DB(t)
	st, err := storagemod.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: st.Pool()})
	if err != nil {
		t.Fatalf("stores: %v", err)
	}

	tm := &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	mustUpsert(t, stores.Team.Upsert(ctx, tm), "team")
	mkProject := func(name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: name}, Spec: project.Spec{TeamID: tm.Meta.ID}}
		p.StampOwner()
		mustUpsert(t, stores.Project.Upsert(ctx, p), "project")
		return p
	}
	bound, unbound := mkProject("ml-search"), mkProject("sandbox")
	mkPolicy := func(name string, p *project.Project) *policy.Policy {
		pol := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}}}
		mustUpsert(t, stores.Policy.Upsert(ctx, pol), "policy")
		return pol
	}
	direct, everyAccount, override := mkPolicy("direct", bound), mkPolicy("every-account", bound), mkPolicy("override", bound)
	mkAccount := func(name string, p *project.Project, policyID string) *serviceaccount.ServiceAccount {
		sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: name}, Spec: serviceaccount.Spec{ProjectID: p.Meta.ID, PolicyID: policyID}}
		sa.StampOwner()
		mustUpsert(t, stores.ServiceAccount.Upsert(ctx, sa), "service account")
		return sa
	}
	indexer := mkAccount("indexer", bound, "")
	reporter := mkAccount("reporter", bound, override.Meta.ID)
	tester := mkAccount("tester", unbound, "")
	mkBinding := func(name string, pol *policy.Policy, priority int, sub rolebinding.Subject) {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: ids.New(), Name: name},
			Spec: policybinding.Spec{ProjectID: bound.Meta.ID, PolicyID: pol.Meta.ID, Priority: &priority, Subjects: []rolebinding.Subject{sub}},
		}
		b.StampOwner()
		mustUpsert(t, stores.PolicyBinding.Upsert(ctx, b), "policy binding")
	}
	// Both bindings match the indexer; the lower priority wins, though it
	// names the account only through the group every account is in.
	mkBinding("indexer-direct", direct, 100, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: indexer.Meta.ID})
	mkBinding("all-accounts", everyAccount, 10, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:serviceaccounts"})

	keys := map[string]*serviceaccount.ServiceAccount{"rk_indexer": indexer, "rk_reporter": reporter, "rk_tester": tester}
	for plaintext, sa := range keys {
		k := &key.Key{
			Meta: meta.Metadata{ID: ids.New(), Name: sa.Meta.Name + "-key", Owner: meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}},
			Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, KeyHash: sha256Hex(plaintext)},
		}
		mustUpsert(t, stores.Key.Upsert(ctx, k), "key")
	}

	// What the data plane resolves before the rollback is what it must write.
	if _, err := cat.Hydrate(ctx, stores, appcatalog.BootstrapOptions{Pool: st.Pool()}); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	want := map[string]string{}
	for plaintext, sa := range keys {
		_, p := authenticate(t, cat, plaintext)
		if p != nil && p.Policy != nil {
			want[sa.Meta.Name+"-key"] = p.Policy.Meta.ID
		} else {
			want[sa.Meta.Name+"-key"] = ""
		}
	}
	if want["indexer-key"] != everyAccount.Meta.ID || want["reporter-key"] != override.Meta.ID || want["tester-key"] != "" {
		t.Fatalf("fixture resolves %v, want indexer→every-account, reporter→override, tester→none", want)
	}

	if err := storagemod.MigrateTo(dsn, 26); err != nil {
		t.Fatalf("migrate down to 26: %v", err)
	}
	for name, policyID := range want {
		var got string
		if err := st.Pool().QueryRow(ctx,
			`SELECT coalesce(spec->>'policyId', '') FROM relay_keys WHERE name = $1`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != policyID {
			t.Errorf("%s: spec.policyId = %q after rollback, want %q", name, got, policyID)
		}
	}
}

// The pre-tenancy admin token wrote a user owner with no id on what it
// created. Nobody owns such a row, so the upgrade hands it to the system: a
// shared host key has to stay in the pool policy-less callers draw from.
func TestUpgradeHandsOwnerlessUserRowsToTheSystem(t *testing.T) {
	u := newUpgradeDB(t)
	ctx := context.Background()
	adminOwned := meta.Owner{Kind: meta.OwnerUser}
	modelName := u.seedRoute(t, adminOwned)

	h := &host.Host{Meta: meta.Metadata{ID: ids.New(), Name: "admin-host", Owner: adminOwned}, Spec: host.Spec{BaseURL: "https://admin.example.com"}}
	mustUpsert(t, u.stores.Host.Upsert(ctx, h), "host")
	md := &model.Model{
		Meta: meta.Metadata{ID: ids.New(), Name: "admin-model", Owner: adminOwned},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "admin-model"}}, Pointer: "admin-model"},
	}
	mustUpsert(t, u.stores.Model.Upsert(ctx, md), "model")
	b := &binding.Binding{
		Meta: meta.Metadata{ID: ids.New(), Name: "admin-model-on-admin-host", Owner: adminOwned},
		Spec: binding.Spec{ModelID: md.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI},
	}
	mustUpsert(t, u.stores.Binding.Upsert(ctx, b), "binding")
	pol := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: "admin-policy", Owner: adminOwned}}
	mustUpsert(t, u.stores.Policy.Upsert(ctx, pol), "policy")
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: ids.New(), Name: "admin-limit", Owner: adminOwned},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: ratelimit.Window(time.Minute)}}},
	}
	mustUpsert(t, u.stores.RateLimit.Upsert(ctx, rl), "rate limit")

	u.upgrade(t)

	for _, table := range []string{"secrets", "policies", "rate_limits", "hosts", "host_bindings", "models"} {
		var ownerless int
		if err := u.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+`
			  WHERE metadata->'owner'->>'kind' = 'user'
			    AND coalesce(metadata->'owner'->>'id', '') = ''`).Scan(&ownerless); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if ownerless != 0 {
			t.Errorf("%s: %d rows still owned by a user with no id, want them system-owned", table, ownerless)
		}
	}

	u.hydrate(t)
	if _, err := routing.New(u.cat).Resolve(routing.Request{ModelName: modelName, Snapshot: u.cat.Current()}); err != nil {
		t.Fatalf("the admin-created host key dropped out of the policy-less pool: %v", err)
	}
}
