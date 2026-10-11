//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/seed"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/password"
	"github.com/wyolet/relay/pkg/ids"
)

// underAuthAPIVersion moves every document of the auth module's kinds to its apiVersion, leaving relay's own kinds (the Policy) where they are: a mixed file. The auth apiVersion refuses metadata.owner, which relay's ignores for these kinds, so it is dropped.
func underAuthAPIVersion(bundle string) string {
	docs := strings.Split(bundle, "---\n")
	for i, d := range docs {
		if !strings.Contains(d, "kind: Policy\n") {
			d = strings.Replace(d, "apiVersion: relay.wyolet.dev/v1alpha2", "apiVersion: auth.wyolet.dev/v1alpha1", 1)
			docs[i] = strings.ReplaceAll(d, ", owner: {kind: user}", "")
		}
	}
	return strings.Join(docs, "---\n")
}

// applyTenancyBundle seeds the built-in roles and the bundle's users, then plans and applies src, returning the plan as "action Kind/name fields" lines and the stored rows by name.
func applyTenancyBundle(t *testing.T, src string) (plan, rows []string) {
	t.Helper()
	st := newStack(t)
	ctx := context.Background()
	if err := role.SeedBuiltins(ctx, st.stores.Role, nil, nil); err != nil {
		t.Fatalf("seed built-in roles: %v", err)
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		if err := st.users.Upsert(ctx, &user.User{ID: ids.New(), Username: name}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := apply.Plan(ctx, parseBundle(t, src), apply.Options{Stores: applyStores(st)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, e := range p.Entries {
		plan = append(plan, string(e.Action)+" "+e.Kind+"/"+e.Name+" "+strings.Join(e.ChangedFields, ","))
	}
	if _, err := apply.Execute(ctx, p, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return plan, tenancyRows(t, st, rowNames(t, st))
}

// The relay apiVersion and the auth module's give the same plan and the same rows for the same documents.
func TestIntegration_AuthKindsWriteTheSameRowsUnderEitherAPIVersion(t *testing.T) {
	t.Parallel()
	mixed := underAuthAPIVersion(tenancyBundle)
	if strings.Count(mixed, "auth.wyolet.dev/v1alpha1") != 12 {
		t.Fatalf("the mixed bundle should move 12 of 13 documents:\n%s", mixed)
	}
	relayPlan, relayRows := applyTenancyBundle(t, tenancyBundle)
	authPlan, authRows := applyTenancyBundle(t, mixed)
	if !slices.Equal(relayPlan, authPlan) {
		t.Errorf("plans differ:\n relay: %v\n auth:  %v", relayPlan, authPlan)
	}
	diff(t, authRows, relayRows)
}

// mixedBundle has relay kinds naming auth kinds (a policy owned by a project, keys for a service account and a user, a policy binding) and auth kinds naming relay kinds (a service account's policy), plus an account; DANA_HASH stands for its password hash.
const mixedBundle = `apiVersion: auth.wyolet.dev/v1alpha1
kind: Team
metadata: {name: data}
spec: {budget: {amount: "10"}}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: Project
metadata: {name: etl}
spec: {team: data}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: etl-default, owner: {kind: project, name: etl}}
spec: {}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: ServiceAccount
metadata: {name: loader}
spec: {project: etl, policy: etl-default}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: User
metadata: {name: dana}
spec:
  email: dana@example.com
  password: {hash: "DANA_HASH"}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Key
metadata: {name: loader-key, owner: {kind: project, name: etl}}
spec: {principal: {kind: serviceaccount, name: loader}, keyHash: "d47712cceb4c780603026e6325221c1bcff90679ebc076baa51c71ebe796717c", prefix: rk-loader}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Key
metadata: {name: dana-key, owner: {kind: user, name: dana}}
spec: {principal: {kind: user, name: dana}, keyHash: "2c9e0a2585dc7406589a3724f0027811506e0f133726303a15d6779d532a2573", prefix: rk-dana}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: PolicyBinding
metadata: {name: etl-users}
spec: {project: etl, policy: etl-default, subjects: [{kind: user, name: dana}, {kind: serviceaccount, name: loader}]}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: RoleBinding
metadata: {name: etl-readers}
spec:
  role: viewer
  scope: {kind: project, name: etl}
  subjects: [{kind: user, name: dana}]
`

func TestIntegration_ApplyMixesRelayAndAuthKinds(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	if err := role.SeedBuiltins(ctx, st.stores.Role, nil, nil); err != nil {
		t.Fatalf("seed built-in roles: %v", err)
	}
	hash, err := password.Hash("a long test password")
	if err != nil {
		t.Fatal(err)
	}
	mixed := strings.Replace(mixedBundle, "DANA_HASH", hash, 1)
	code, plan, raw := st.applyBundle(mixed, "")
	if code != http.StatusOK {
		t.Fatalf("apply = %d: %s", code, raw)
	}
	if got := plan.Counts.Create; got != 9 {
		t.Fatalf("created %d rows, want 9: %s", got, raw)
	}
	dana, err := st.users.ByUsername(ctx, "dana")
	if err != nil || dana == nil {
		t.Fatalf("dana = %v, %v", dana, err)
	}
	if dana.PasswordHash != hash {
		t.Errorf("dana's password hash was not stored as given")
	}
	keys, err := st.stores.Key.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Meta.Name == "dana-key" && (k.Spec.Principal.Kind != key.PrincipalUser || k.Spec.Principal.ID != dana.ID) {
			t.Errorf("dana-key principal = %+v, want user %s", k.Spec.Principal, dana.ID)
		}
	}
	sas, err := st.stores.ServiceAccount.List(ctx)
	if err != nil || len(sas) != 1 || sas[0].Spec.PolicyID == "" {
		t.Fatalf("service accounts = %+v, %v", sas, err)
	}

	// Re-applying the same file changes nothing.
	_, again, raw := st.applyBundle(mixed, "dryRun=true")
	if again.Counts.Unchanged != 9 {
		t.Fatalf("re-apply: %s", raw)
	}

	// Export renders the auth kinds back with relay's names, and the export applies as unchanged.
	code, body := st.adminDo(http.MethodGet, "/api/export", "")
	if code != http.StatusOK {
		t.Fatalf("export = %d: %s", code, body)
	}
	if !strings.Contains(string(body), "policy: etl-default") || !strings.Contains(string(body), "team: data") {
		t.Errorf("export lacks the service account's policy or the project's team:\n%s", body)
	}
	_, fromExport, raw := st.applyBundle(string(body), "dryRun=true")
	if fromExport.Counts.Create != 0 || fromExport.Counts.Update != 0 {
		t.Errorf("export re-applied with changes: %s", raw)
	}
}

// Under the auth apiVersion a typo in a rule fails the apply naming the document and the field, and so does a plain-text password.
func TestIntegration_ApplyRefusesATypoInARule(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	bundle := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: t}\n---\n" +
		"apiVersion: auth.wyolet.dev/v1alpha1\nkind: Role\nmetadata: {name: model-editor}\nspec: {rules: [{kinds: [models], verbs: [updat]}]}\n"
	code, _, raw := st.applyBundle(bundle, "dryRun=true")
	if code != http.StatusBadRequest || !strings.Contains(string(raw), `doc 1 Role \"model-editor\": spec.rules[0].verbs[0]: \"updat\" is not a registered verb`) {
		t.Fatalf("apply = %d %s", code, raw)
	}
	code, _, raw = st.applyBundle("apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: eve}\nspec: {password: hunter22}\n", "dryRun=true")
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "plain-text passwords are refused") {
		t.Fatalf("plain password apply = %d %s", code, raw)
	}
}

// envUser reads its password from the environment, which only `relay seed` may do.
const envUser = "apiVersion: auth.wyolet.dev/v1alpha1\nkind: User\nmetadata: {name: erin}\nspec: {password: {valueFrom: {env: RELAY_TEST_ERIN_PASSWORD}}}\n"

// The API refuses an environment-sourced password, so a caller cannot read the server's variables into an account.
func TestIntegration_ApplyRefusesAnEnvPassword(t *testing.T) {
	t.Setenv("RELAY_TEST_ERIN_PASSWORD", "a long test password")
	st := newStack(t)
	code, _, raw := st.applyBundle(envUser, "dryRun=true")
	if code != http.StatusBadRequest || !strings.Contains(string(raw), "password.valueFrom.env reads the server's environment") {
		t.Fatalf("apply = %d %s", code, raw)
	}
	if u, err := st.users.ByUsername(context.Background(), "erin"); err != nil || u != nil {
		t.Fatalf("erin = %+v, %v; want no account", u, err)
	}
}

// `relay seed` runs as the operator, so it reads the password from its own environment and hashes it.
func TestIntegration_SeedReadsAnEnvPassword(t *testing.T) {
	t.Setenv("RELAY_TEST_ERIN_PASSWORD", "a long test password")
	st := newStack(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "erin.yaml"), []byte(envUser), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := seed.Run(context.Background(), seed.Options{Pool: testPool(t, st.dsn), YAMLDir: dir})
	if err != nil || res.Users != 1 {
		t.Fatalf("seed = %+v, %v", res, err)
	}
	u, err := st.users.ByUsername(context.Background(), "erin")
	if err != nil || u == nil {
		t.Fatalf("erin = %v, %v", u, err)
	}
	if ok, plain := password.Verify(u.PasswordHash, "a long test password"); !ok || plain {
		t.Errorf("erin's password is not a bcrypt hash of the env value")
	}
}
