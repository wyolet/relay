package routing

// Policy-less resolution: the gate that admits it and the plan it builds. The
// key pool it draws on is TestResolvePolicyless_KeyPoolScope; the listing's
// agreement with the flow is TestPolicylessAllows_MatchesTheFlowThatServesIt.

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
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/slug"
)

// openPolicyless answers the inference section with policy-less traffic
// switched on, which is the only setting the gate reads.
type openPolicyless struct{}

func (openPolicyless) Setting(section string) (any, bool) {
	if section != settings.SectionInference {
		return nil, false
	}
	return &settings.Inference{AllowMissingPolicy: true}, true
}

// Policy-less traffic is off unless the operator switched it on, so a caller
// that resolved no policy is refused rather than served from the shared pool
// by default.
func TestResolve_PolicylessIsClosedByDefault(t *testing.T) {
	f := newTwoHostParts()
	c := catalog.New(
		lister[provider.Provider]{f.provider}, lister[host.Host]{f.hostRowA},
		lister[policy.Policy]{f.tierA}, lister[model.Model]{f.model},
		lister[hostkey.HostKey]{f.keyA}, lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{f.bindingA},
	)
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := New(c).Resolve(Request{ModelName: "m1"}); !errors.Is(err, ErrPolicyless) {
		t.Fatalf("err = %v, want ErrPolicyless", err)
	}
}

// The operator setting opens policy-less traffic only under
// single-user authorization. Under rbac the grants a credential carries are
// the whole access model, so a key whose policy does not resolve is refused
// with the same missing-policy error the setting-off path answers.
func TestResolve_PolicylessHonouredOnlyUnderSingleAuthorization(t *testing.T) {
	f := newTwoHostParts()
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)

	single := &Resolver{cfg: openPolicyless{}}
	if !single.PolicylessTrafficAllowed() {
		t.Fatal("the setting is on and authorization is single, but the gate is closed")
	}
	if _, err := single.Resolve(Request{ModelName: "m1", Snapshot: snap}); err != nil {
		t.Fatalf("Resolve under single authorization: %v", err)
	}

	rbac := &Resolver{cfg: openPolicyless{}, requirePolicy: true}
	if rbac.PolicylessTrafficAllowed() {
		t.Error("the listing would advertise models to a caller the flow refuses")
	}
	if _, err := rbac.Resolve(Request{ModelName: "m1", Snapshot: snap}); !errors.Is(err, ErrPolicyless) {
		t.Fatalf("err = %v, want ErrPolicyless — rbac served a key with no policy", err)
	}
}

func TestResolvePolicyless_NoAuthHostInjectsAnonKey(t *testing.T) {
	provID, hostID, modID := meta.NewID(), meta.NewID(), meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "ollama", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "ollama-self", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://localhost:11434", NoAuth: true},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "qwen3", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "qwen3"}}, Pointer: slug.From("qwen3")},
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "qwen3-on-ollama", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	snap := catalog.Build([]*provider.Provider{prov}, []*host.Host{h}, nil, nil, []*model.Model{m}, nil, nil, nil, []*binding.Binding{b})

	plan, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{m}, &m.Spec.Snapshots[0], "", "")
	if err != nil {
		t.Fatalf("resolvePolicyless NoAuth host with no real keys: %v", err)
	}
	if plan.Policy != nil {
		t.Fatalf("policyless plan Policy = %+v, want nil", plan.Policy)
	}
	if plan.Host.Meta.ID != hostID {
		t.Fatalf("plan host = %q, want %q", plan.Host.Meta.ID, hostID)
	}
	if len(plan.Keys) != 1 {
		t.Fatalf("injected keys = %d, want 1", len(plan.Keys))
	}
	if k := plan.Keys[0]; k.Spec.HostID != hostID || k.Resolved != "" || k.KeyHash != hostkey.AnonIDPrefix+hostID {
		t.Fatalf("anon key = {host:%q resolved:%q hash:%q}, want host %q, empty value, hash %q", k.Spec.HostID, k.Resolved, k.KeyHash, hostID, hostkey.AnonIDPrefix+hostID)
	}
}
