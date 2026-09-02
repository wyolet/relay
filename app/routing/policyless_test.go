package routing

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

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
