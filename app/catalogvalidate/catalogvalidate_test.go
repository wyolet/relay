package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/model"
)

// fixture is a tiny in-memory catalog scaffold; tests start from a clean
// 1-provider/1-host/1-model graph and mutate from there. Avoids YAML
// loading noise; the loader's own tests cover that.
func fixture() []manifest.Document {
	return []manifest.Document{
		{Provider: &manifest.ProviderDTO{
			APIVersion: "v1alpha2",
			Kind:       "Provider",
			Metadata:   manifest.WireMeta{Name: "openai", Owner: manifest.WireOwner{Kind: "system"}},
		}},
		{Host: &manifest.HostDTO{
			APIVersion: "v1alpha2",
			Kind:       "Host",
			Metadata:   manifest.WireMeta{Name: "openai-host", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:       manifest.HostSpec{BaseURL: "https://api.openai.com"},
		}},
		{Model: &manifest.ModelDTO{
			APIVersion: "v1alpha2",
			Kind:       "Model",
			Metadata:   manifest.WireMeta{Name: "gpt-x", Owner: manifest.WireOwner{Kind: "provider", Name: "openai"}},
			Spec: manifest.ModelSpec{
				Hosts: []manifest.HostBindingDTO{
					{Host: "openai-host", Adapter: "openai"},
				},
				Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}},
				Pointer:   "gpt-x",
			},
		}},
	}
}

func TestValidateGraph_CleanFixture(t *testing.T) {
	issues := ValidateGraph(fixture())
	// The minimal fixture has no policies/hostkeys/ratelimits so we
	// shouldn't fail on errors. Warnings are fine.
	if HasErrors(issues) {
		t.Fatalf("clean fixture should have no errors, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_DuplicateModelName(t *testing.T) {
	docs := fixture()
	// Add a second model with the same name.
	docs = append(docs, manifest.Document{
		Model: &manifest.ModelDTO{
			APIVersion: "v1alpha2",
			Kind:       "Model",
			Metadata:   manifest.WireMeta{Name: "gpt-x", Owner: manifest.WireOwner{Kind: "provider", Name: "openai"}},
			Spec: manifest.ModelSpec{
				Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}},
			},
		},
	})
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindDuplicateName) {
		t.Fatalf("expected duplicate_name issue, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_ModelMissingHost(t *testing.T) {
	docs := fixture()
	// Re-point the model's host binding at a host that doesn't exist.
	docs[2].Model.Spec.Hosts[0].Host = "no-such-host"
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "Host", "no-such-host") {
		t.Fatalf("expected ref_missing → Host/no-such-host, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_HostBindingSnapshotNotInModel(t *testing.T) {
	docs := fixture()
	// Host binding references a snapshot the Model doesn't declare.
	docs[2].Model.Spec.Hosts[0].Snapshots = []string{"gpt-x-typo"}
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindSnapshotMissing) {
		t.Fatalf("expected snapshot_missing, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_ModelNoSnapshots(t *testing.T) {
	docs := fixture()
	docs[2].Model.Spec.Snapshots = nil
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindIncomplete) {
		t.Fatalf("expected incomplete (no snapshots), got:\n%s", Format(issues))
	}
}

func TestValidateGraph_HostKeyOwnerMismatch(t *testing.T) {
	docs := fixture()
	// Add a second host + a policy owned by it + a hostkey claiming the
	// first host but pointing at the policy owned by the second.
	docs = append(docs,
		manifest.Document{Host: &manifest.HostDTO{
			Metadata: manifest.WireMeta{Name: "anthropic-host", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:     manifest.HostSpec{BaseURL: "https://api.anthropic.com"},
		}},
		manifest.Document{Policy: &manifest.PolicyDTO{
			Metadata: manifest.WireMeta{Name: "anthropic-tier-1", Owner: manifest.WireOwner{Kind: "host", Name: "anthropic-host"}},
		}},
		manifest.Document{HostKey: &manifest.HostKeyDTO{
			Metadata: manifest.WireMeta{Name: "bad-hostkey"},
			Spec: manifest.HostKeySpec{
				HostID:    "openai-host",       // claims openai-host
				PolicyID:  "anthropic-tier-1",  // but uses anthropic policy
				ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "X"},
			},
		}},
	)
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindOwnerMismatch) {
		t.Fatalf("expected owner_mismatch, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_PolicyMissingHostKey(t *testing.T) {
	docs := fixture()
	docs = append(docs, manifest.Document{Policy: &manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "user-pol", Owner: manifest.WireOwner{Kind: "user"}},
		Spec:     manifest.PolicySpec{HostKeys: []string{"ghost-key"}},
	}})
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "HostKey", "ghost-key") {
		t.Fatalf("expected ref_missing → HostKey/ghost-key, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_PolicyMissingRateLimit(t *testing.T) {
	docs := fixture()
	docs = append(docs, manifest.Document{Policy: &manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "rl-pol", Owner: manifest.WireOwner{Kind: "user"}},
		Spec:     manifest.PolicySpec{RateLimit: "ghost-rl"},
	}})
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "RateLimit", "ghost-rl") {
		t.Fatalf("expected ref_missing → RateLimit/ghost-rl, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_PricingMissingTargetModel(t *testing.T) {
	docs := fixture()
	docs = append(docs, manifest.Document{Pricing: &manifest.PricingDTO{
		Metadata: manifest.WireMeta{Name: "p1", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
		Spec: manifest.PricingSpec{
			Currency:     "USD",
			TargetModels: []string{"ghost-model"},
		},
	}})
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "Model", "ghost-model") {
		t.Fatalf("expected ref_missing → Model/ghost-model, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_RelayKeyMissingPolicy(t *testing.T) {
	docs := fixture()
	docs = append(docs, manifest.Document{RelayKey: &manifest.RelayKeyDTO{
		Metadata: manifest.WireMeta{Name: "k1"},
		Spec:     manifest.RelayKeySpec{Policy: "ghost-policy", KeyHash: "x"},
	}})
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "Policy", "ghost-policy") {
		t.Fatalf("expected ref_missing → Policy/ghost-policy, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_HostDefaultPolicyMissing(t *testing.T) {
	docs := fixture()
	docs[1].Host.Spec.DefaultPolicy = "ghost-policy"
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "Policy", "ghost-policy") {
		t.Fatalf("expected ref_missing → Policy/ghost-policy on Host.spec.defaultPolicy, got:\n%s", Format(issues))
	}
}

func TestFormat_NoIssues(t *testing.T) {
	got := Format(nil)
	if !strings.Contains(got, "catalog ok") {
		t.Fatalf("expected ok message, got %q", got)
	}
}

// hasIssue reports whether issues contains an Issue with the given kind.
func hasIssue(issues []Issue, kind IssueKind) bool {
	for _, i := range issues {
		if i.Kind == kind {
			return true
		}
	}
	return false
}

// hasRefMissing checks for a ref_missing issue with a specific target.
func hasRefMissing(issues []Issue, targetKind, targetName string) bool {
	for _, i := range issues {
		if i.Kind == KindRefMissing && i.Target.Kind == targetKind && i.Target.Name == targetName {
			return true
		}
	}
	return false
}
