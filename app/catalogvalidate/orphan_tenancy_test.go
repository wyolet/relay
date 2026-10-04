package catalogvalidate

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

// A project-owned Policy routes through its host keys exactly like a
// user-owned one. TestOrphanCountsTenantOwnedPolicies pins that a key one of
// them lists is not reported unreachable.
func TestOrphanCountsTenantOwnedPolicies(t *testing.T) {
	hostKey := manifest.Document{HostKey: &manifest.HostKeyDTO{
		Metadata: manifest.WireMeta{Name: "openai-key", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
		Spec: manifest.HostKeySpec{
			HostID:    "openai-host",
			PolicyID:  "team-pol",
			ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "OPENAI_API_KEY"},
		},
	}}

	orphaned := func(docs []manifest.Document) bool {
		for _, i := range ValidateGraph(docs) {
			if i.Kind == KindOrphan && i.Source.Kind == "HostKey" && i.Source.Name == "openai-key" {
				return true
			}
		}
		return false
	}

	// Referenced by a project-owned policy: reachable.
	docs := append(fixture(), hostKey, manifest.Document{Policy: &manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "team-pol", Owner: manifest.WireOwner{Kind: "project", Name: "ml-search"}},
		Spec:     manifest.PolicySpec{HostKeys: []string{"openai-key"}},
	}})
	if orphaned(docs) {
		t.Fatal("a host key listed by a project-owned policy was reported unreachable")
	}

	// Referenced by nothing: still reported.
	if !orphaned(append(fixture(), hostKey)) {
		t.Fatal("a host key no policy lists must still be reported")
	}
}
