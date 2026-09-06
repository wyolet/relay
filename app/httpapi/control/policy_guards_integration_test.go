//go:build integration

// policy_guards_integration_test.go covers the policy-reference guards
// against a real Postgres: they read through concrete *policy.Store /
// *hostkey.Store / *ratelimit.Store, which have no fake seam.
package control

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// D74: a binder may name only its own project's policy or a
// system-owned shared one; another project's policy and a host tier policy
// are both refused, naming the offending row.
func TestCheckPolicyRefVisible_D74OwnerRule(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ref-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	mine, theirs := meta.NewID(), meta.NewID()
	own := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "own-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: mine}}}
	foreign := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "other-project-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: theirs}}}
	shared := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-policy", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	for _, p := range []*policy.Policy{own, foreign, shared, tier} {
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert policy %s: %v", p.Meta.Name, err)
		}
	}

	binder := meta.Owner{Kind: meta.OwnerProject, ID: mine}
	if err := checkPolicyRefVisible(ctx, d, own.Meta.ID, binder); err != nil {
		t.Errorf("own project policy refused: %v", err)
	}
	if err := checkPolicyRefVisible(ctx, d, shared.Meta.ID, binder); err != nil {
		t.Errorf("system-owned shared policy refused: %v", err)
	}
	err = checkPolicyRefVisible(ctx, d, foreign.Meta.ID, binder)
	if statusOf(t, err) != 400 || !strings.Contains(err.Error(), foreign.Meta.Name) {
		t.Errorf("foreign project policy: err = %v, want a 400 naming %q", err, foreign.Meta.Name)
	}
	err = checkPolicyRefVisible(ctx, d, tier.Meta.ID, binder)
	if statusOf(t, err) != 400 || !strings.Contains(err.Error(), "tier policy") {
		t.Errorf("host tier policy: err = %v, want a 400 calling it a tier policy", err)
	}
}

// A policyId naming no row is a bad request, not a silently dropped
// reference the caller never hears about.
func TestCheckPolicyRefVisible_DanglingIsA400(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	err = checkPolicyRefVisible(visibleCtx(ctx), d, meta.NewID(), meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})
	if statusOf(t, err) != 400 || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a 400 saying the policy does not exist", err)
	}
}

// D74: RateLimit refs on a policy follow the same owner rule.
func TestCheckRateLimitRefVisible_D74OwnerRule(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)

	rules := []ratelimit.Rule{{
		Meter: ratelimit.MeterRequests, Amount: 10,
		Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow,
	}}
	mine, theirs := meta.NewID(), meta.NewID()
	own := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "own-limit", Owner: meta.Owner{Kind: meta.OwnerProject, ID: mine}},
		Spec: ratelimit.Spec{Rules: rules},
	}
	foreign := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-project-limit", Owner: meta.Owner{Kind: meta.OwnerProject, ID: theirs}},
		Spec: ratelimit.Spec{Rules: rules},
	}
	for _, rl := range []*ratelimit.RateLimit{own, foreign} {
		if err := stores.RateLimit.Upsert(ctx, rl); err != nil {
			t.Fatalf("upsert rate limit %s: %v", rl.Meta.Name, err)
		}
	}
	binder := meta.Owner{Kind: meta.OwnerProject, ID: mine}
	if err := checkRateLimitRefVisible(ctx, d, own.Meta.ID, binder); err != nil {
		t.Errorf("own project rate limit refused: %v", err)
	}
	err = checkRateLimitRefVisible(ctx, d, foreign.Meta.ID, binder)
	if statusOf(t, err) != 400 || !strings.Contains(err.Error(), foreign.Meta.Name) {
		t.Errorf("foreign rate limit: err = %v, want a 400 naming %q", err, foreign.Meta.Name)
	}
	err = checkRateLimitRefVisible(ctx, d, meta.NewID(), binder)
	if statusOf(t, err) != 400 || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("dangling rate limit: err = %v, want a 400", err)
	}

	// D51/D70: a personal policy must not meter itself against a project's
	// limits either — the rule the policy ref already applied.
	personal := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	outsider := actor.WithActor(ctx, &actor.Actor{UserID: "u-alice", Username: "alice"})
	err = checkRateLimitRefVisible(outsider, d, own.Meta.ID, personal)
	if err == nil {
		t.Error("a personal row referenced a project's rate limit")
	} else if got := err.Error(); !strings.Contains(got, "personal rows cannot reference project resources") &&
		!strings.Contains(got, "not found") {
		t.Errorf("err = %q, want the reference refused", got)
	}
}

// D76: deleting a policy host keys mirror as their tier is refused
// with a 409 naming them: clearing the ref would leave every one invalid.
func TestGuardPolicyDelete_RefusedWhileHostKeysUseTheTier(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)
	t.Setenv("TIER_HOSTKEY_VALUE", "sk-test-value")

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-in-use", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	unused := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-unused", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	for _, p := range []*policy.Policy{tier, unused} {
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert policy %s: %v", p.Meta.Name, err)
		}
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, PolicyID: tier.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "TIER_HOSTKEY_VALUE"}},
	}
	if err := stores.HostKey.Upsert(ctx, hk); err != nil {
		t.Fatalf("upsert host-key: %v", err)
	}

	err = guardPolicyModels(d)(ctx, "delete", tier, nil)
	if statusOf(t, err) != 409 || !strings.Contains(err.Error(), hk.Meta.Name) {
		t.Fatalf("err = %v, want a 409 naming host key %q", err, hk.Meta.Name)
	}
	if err := guardPolicyModels(d)(ctx, "delete", unused, nil); err != nil {
		t.Fatalf("deleting a policy no host key mirrors was refused: %v", err)
	}
}

// The delete cascade clears the ServiceAccount override too —
// it used to leave accounts pointing at a row that was about to vanish.
func TestCascadePolicyDetach_ClearsServiceAccountOverride(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "detach-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "detach-project"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	if err := stores.Project.Upsert(ctx, proj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "detach-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}}}
	if err := stores.Policy.Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	sa := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "detach-account"},
		Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID, PolicyID: pol.Meta.ID},
	}
	sa.StampOwner()
	if err := stores.ServiceAccount.Upsert(ctx, sa); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}

	if err := cascadePolicyDetach(d)(ctx, pol); err != nil {
		t.Fatalf("cascadePolicyDetach: %v", err)
	}
	got, err := stores.ServiceAccount.Get(ctx, sa.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("read back service account: %v", err)
	}
	if got.Spec.PolicyID != "" {
		t.Fatalf("service account still points at the deleted policy %q", got.Spec.PolicyID)
	}
}

// Attaching a key through the policy sub-resource runs the same guard,
// Validate and dirty flag a PUT on the key runs.
func TestWriteKeyPolicy_RunsTheGuardAndMarksDirty(t *testing.T) {
	pool, ctx := setupPolicyRefDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	d := Deps{Authz: testRBAC(), Stores: stores}
	ctx = visibleCtx(ctx)

	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := stores.Host.Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hst.Meta.ID}}}
	shared := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-shared", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	for _, p := range []*policy.Policy{tier, shared} {
		if err := stores.Policy.Upsert(ctx, p); err != nil {
			t.Fatalf("upsert policy %s: %v", p.Meta.Name, err)
		}
	}
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-team", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Team.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-project"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	if err := stores.Project.Upsert(ctx, proj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	sa := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-account"},
		Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID},
	}
	sa.StampOwner()
	if err := stores.ServiceAccount.Upsert(ctx, sa); err != nil {
		t.Fatalf("upsert service account: %v", err)
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "attach-key", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID},
			KeyHash:   strings.Repeat("a", 64), Prefix: "sk-attach",
		},
	}
	if err := stores.Key.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert key: %v", err)
	}

	// A tier policy is not bindable — the guard the endpoint used to bypass.
	k.Spec.PolicyID = tier.Meta.ID
	if err := writeKeyPolicy(ctx, d, k); statusOf(t, err) != 400 {
		t.Fatalf("attaching a tier policy: err = %v, want a 400", err)
	}

	k.Spec.PolicyID = shared.Meta.ID
	k.Meta.Dirty = false
	if err := writeKeyPolicy(ctx, d, k); err != nil {
		t.Fatalf("writeKeyPolicy: %v", err)
	}
	got, err := stores.Key.Get(ctx, k.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("read back key: %v", err)
	}
	if got.Spec.PolicyID != shared.Meta.ID {
		t.Fatalf("policyId = %q, want %q", got.Spec.PolicyID, shared.Meta.ID)
	}
	if !got.Meta.Dirty {
		t.Fatal("the attach did not mark the row hand-edited; apply would overwrite it silently")
	}
}
