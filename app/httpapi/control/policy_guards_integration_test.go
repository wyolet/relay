//go:build integration

// policy_guards_integration_test.go covers the policy-reference guards
// against a real Postgres: they read through concrete *policy.Store /
// *ratelimit.Store, which have no fake seam.

package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
)

func checkPolicyRefVisible(ctx context.Context, d Deps, policyID string, refOwner meta.Owner) error {
	return refErr(refs(d).PolicyRef(ctx, policyID, refOwner))
}

func checkRateLimitRefVisible(ctx context.Context, d Deps, rateLimitID string, refOwner meta.Owner) error {
	return refErr(refs(d).RateLimitRef(ctx, rateLimitID, refOwner))
}

// A binder may name only its own project's policy or a system-owned shared
// one; another project's policy and a host tier policy are both refused,
// naming the offending row.
func TestCheckPolicyRefVisible_OwnProjectOrSharedOnly(t *testing.T) {
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

// A policy id naming no row is a bad request, not a silently dropped
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

// RateLimit refs on a policy follow the same owner rule.
func TestCheckRateLimitRefVisible_OwnProjectOrSharedOnly(t *testing.T) {
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

	// A personal policy must not meter itself against a project's limits
	// either, matching the rule for policy refs.
	personal := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	outsider := actor.WithActor(ctx, &actor.Actor{UserID: "u-alice", Username: "alice"})
	err = checkRateLimitRefVisible(outsider, d, own.Meta.ID, personal)
	if err == nil {
		t.Error("a personal row referenced a project's rate limit")
	} else if got := err.Error(); !strings.Contains(got, "a personal row uses a project's rows only where you may create keys") &&
		!strings.Contains(got, "not found") {
		t.Errorf("err = %q, want the reference refused", got)
	}
}
