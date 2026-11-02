package catalogvalidate

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

// A project-owned Policy routes through its credentials and pools exactly
// like a user-owned one. TestOrphanCountsTenantOwnedPolicies pins that a key
// one of them links, directly or through a pool, is not reported unreachable.
func TestOrphanCountsTenantOwnedPolicies(t *testing.T) {
	hostKey := manifest.Document{HostKey: &manifest.HostKeyDTO{
		Metadata: manifest.WireMeta{Name: "openai-key", Owner: manifest.WireOwner{Kind: "system"}},
		Spec: manifest.HostKeySpec{
			HostID:    "openai-host",
			ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "OPENAI_API_KEY"},
		},
	}}
	pool := manifest.Document{Pool: &manifest.PoolDTO{
		Metadata: manifest.WireMeta{Name: "openai-pool", Owner: manifest.WireOwner{Kind: "system"}},
		Spec:     manifest.PoolSpec{Host: "openai-host", Credentials: []string{"openai-key"}},
	}}
	policyLinking := func(spec manifest.PolicySpec) manifest.Document {
		return manifest.Document{Policy: &manifest.PolicyDTO{
			Metadata: manifest.WireMeta{Name: "team-pol", Owner: manifest.WireOwner{Kind: "project", Name: "ml-search"}},
			Spec:     spec,
		}}
	}

	orphaned := func(docs []manifest.Document) bool {
		for _, i := range ValidateGraph(docs) {
			if i.Kind == KindOrphan && i.Source.Kind == "HostKey" && i.Source.Name == "openai-key" {
				return true
			}
		}
		return false
	}

	if orphaned(append(fixture(), hostKey, pool, policyLinking(manifest.PolicySpec{Pools: []string{"openai-pool"}}))) {
		t.Fatal("a host key in a pool a project-owned policy links was reported unreachable")
	}
	if !orphaned(append(fixture(), hostKey)) {
		t.Fatal("a host key no policy links must still be reported")
	}
}
