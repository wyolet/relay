package routing_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/slug"
)

// TestResolve_WildcardPolicyDoesNotReachUngrantedNoAuthHost documents audit
// 2026-07-04 (audit-app-services.md, "wildcard × NoAuth authz widening",
// routing.go:262-284 DECISION item).
//
// An implicit-wildcard policy (no ModelIDs, no Models) is documented as
// granting "every model reachable through the policy's hostkeys" — the
// hostkey-coverage check is the real authorization gate (routing.go step-3
// comment, policy.Spec.Models doc). This policy's only hostkey belongs to
// a DIFFERENT host, so the model served exclusively by the NoAuth host is
// outside the grant and Resolve must refuse to route to it.
//
// Today routing.Resolve injects the synthetic anonymous key for any NoAuth
// host BEFORE consulting Policy.Spec.HostKeyIDs, so every relay key on any
// wildcard policy reaches every model on every NoAuth host — an
// authorization widening the operator never granted.
func TestResolve_WildcardPolicyDoesNotReachUngrantedNoAuthHost(t *testing.T) {
	provID := meta.NewID()
	keyedHostID := meta.NewID()
	openHostID := meta.NewID()
	hkID := meta.NewID()
	openModelID := meta.NewID()
	polID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	keyedHost := &host.Host{
		Meta: meta.Metadata{ID: keyedHostID, Name: "keyed-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://keyed.example"},
	}
	openHost := &host.Host{
		Meta: meta.Metadata{ID: openHostID, Name: "open-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://open.example", NoAuth: true},
	}
	// The policy's ONLY grant: a key that authenticates against keyed-host.
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "keyed-k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: keyedHostID}},
		Spec: hostkey.Spec{HostID: keyedHostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	// A model served ONLY by the NoAuth host the policy was never granted.
	openModel := &model.Model{
		Meta: meta.Metadata{ID: openModelID, Name: "open-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "open-model"}},
			Pointer:   slug.From("open-model"),
		},
	}
	openBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "open-model-on-open-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: openModelID, HostID: openHostID, Adapter: adapters.OpenAI},
	}
	// Implicit wildcard: neither ModelIDs nor Models. Documented gate =
	// "models reachable via HostKeyIDs" — which cover keyed-host only.
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "wildcard-pol", Owner: meta.Owner{Kind: meta.OwnerHost, ID: keyedHostID}},
		Spec: policy.Spec{HostKeyIDs: []string{hkID}},
	}
	rk := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "h"},
	}

	c := catalog.New(
		provListR{prov},
		hostListR{keyedHost, openHost},
		polListR{pol},
		modListR{openModel},
		keyListR{hk},
		rlListR{},
		rkListR{rk},
		rcListR{},
		bndListR{openBnd},
	)
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	r := routing.New(c)

	plan, err := r.Resolve(routing.Request{ModelName: "open-model", Key: rk})
	if err == nil {
		t.Fatalf("authz widening: wildcard policy with zero grants for host %q routed to it anyway: plan host=%q model=%q keys=%d (first key hash %q); want an error (model outside the policy's hostkey coverage)",
			openHost.Meta.Name, plan.Host.Meta.Name, plan.Model.Meta.Name, len(plan.Keys), plan.Keys[0].KeyHash)
	}
	if !errors.Is(err, routing.ErrModelNotInPolicy) {
		t.Fatalf("gated NoAuth route returned %v, want ErrModelNotInPolicy (same shape as any other coverage miss)", err)
	}
}

// TestResolve_ExplicitModelGrantReachesNoAuthHost is the positive companion:
// the gate blocks only the implicit wildcard. A policy that explicitly grants
// the model (Models DSL ref — same machinery covers provider-level grants)
// still routes to the NoAuth host, with the synthetic anonymous key injected
// even though the policy holds zero HostKeyIDs.
func TestResolve_ExplicitModelGrantReachesNoAuthHost(t *testing.T) {
	provID := meta.NewID()
	openHostID := meta.NewID()
	openModelID := meta.NewID()
	polID := meta.NewID()

	prov := &provider.Provider{
		Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	openHost := &host.Host{
		Meta: meta.Metadata{ID: openHostID, Name: "open-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://open.example", NoAuth: true},
	}
	openModel := &model.Model{
		Meta: meta.Metadata{ID: openModelID, Name: "open-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "open-model"}},
			Pointer:   slug.From("open-model"),
		},
	}
	openBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "open-model-on-open-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: openModelID, HostID: openHostID, Adapter: adapters.OpenAI},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "explicit-pol", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{Models: []string{"acme/open-model"}},
	}
	rk := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "h"},
	}

	c := catalog.New(
		provListR{prov},
		hostListR{openHost},
		polListR{pol},
		modListR{openModel},
		keyListR{},
		rlListR{},
		rkListR{rk},
		rcListR{},
		bndListR{openBnd},
	)
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	r := routing.New(c)

	plan, err := r.Resolve(routing.Request{ModelName: "open-model", Key: rk})
	if err != nil {
		t.Fatalf("explicit model grant should reach the NoAuth host, got %v", err)
	}
	if plan.Host.Meta.ID != openHostID {
		t.Fatalf("plan host = %q, want %q", plan.Host.Meta.ID, openHostID)
	}
	if len(plan.Keys) != 1 {
		t.Fatalf("injected keys = %d, want 1 anon key", len(plan.Keys))
	}
	if k := plan.Keys[0]; k.Resolved != "" || k.KeyHash != hostkey.AnonIDPrefix+openHostID {
		t.Fatalf("anon key = {resolved:%q hash:%q}, want empty value + hash %q", k.Resolved, k.KeyHash, hostkey.AnonIDPrefix+openHostID)
	}
}
