package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/model"
)

// fixture is a tiny in-memory catalog scaffold; tests start from a clean
// 1-provider/1-host/1-model/1-hostbinding graph and mutate from there.
// Avoids YAML loading noise; the loader's own tests cover that.
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
				Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}},
				Pointer:   "gpt-x",
			},
		}},
		{HostBinding: &manifest.HostBindingDTO{
			APIVersion: "v1alpha2",
			Kind:       "HostBinding",
			Metadata:   manifest.WireMeta{Name: "gpt-x-openai-host", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:       manifest.HostBindingSpec{Model: "gpt-x", Host: "openai-host", Adapter: "openai"},
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

func TestValidateGraph_BindingMissingHost(t *testing.T) {
	docs := fixture()
	// Re-point the standalone binding at a host that doesn't exist.
	docs[3].HostBinding.Spec.Host = "no-such-host"
	issues := ValidateGraph(docs)
	if !hasRefMissing(issues, "Host", "no-such-host") {
		t.Fatalf("expected ref_missing → Host/no-such-host, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_BindingSnapshotNotInModel(t *testing.T) {
	docs := fixture()
	// Standalone binding references a snapshot the Model doesn't declare.
	docs[3].HostBinding.Spec.Snapshots = []string{"gpt-x-typo"}
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindSnapshotMissing) {
		t.Fatalf("expected snapshot_missing, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_DuplicateModelHostBinding(t *testing.T) {
	docs := fixture()
	dup := *docs[3].HostBinding
	dup.Metadata.Name = "gpt-x-openai-host-2"
	docs = append(docs, manifest.Document{HostBinding: &dup})
	issues := ValidateGraph(docs)
	for _, is := range issues {
		if is.Kind == KindInvariant && strings.Contains(is.Message, "duplicate (model=") {
			return
		}
	}
	t.Fatalf("expected duplicate (model, host) issue, got:\n%s", Format(issues))
}

func TestValidateGraph_BindingAdapter(t *testing.T) {
	for adapter, wantErr := range map[string]bool{
		"": false, "openai_responses": false, "anthropic": false,
		"canonical": true, "openai_embeddings": true, "no-such-adapter": true,
	} {
		docs := fixture()
		docs[3].HostBinding.Spec.Adapter = adapter
		issues := ValidateGraph(docs)
		if HasErrors(issues) != wantErr {
			t.Errorf("adapter %q: errors = %v, want %v:\n%s", adapter, HasErrors(issues), wantErr, Format(issues))
		}
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

// A host key's account must be held at the key's own host.
func TestValidateGraph_HostKeyAccountAtAnotherHost(t *testing.T) {
	docs := fixture()
	docs = append(docs,
		manifest.Document{Host: &manifest.HostDTO{
			Metadata: manifest.WireMeta{Name: "anthropic-host", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:     manifest.HostSpec{BaseURL: "https://api.anthropic.com"},
		}},
		manifest.Document{Account: &manifest.AccountDTO{
			Metadata: manifest.WireMeta{Name: "acme-anthropic", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:     manifest.AccountSpec{Host: "anthropic-host"},
		}},
		manifest.Document{HostKey: &manifest.HostKeyDTO{
			Metadata: manifest.WireMeta{Name: "bad-hostkey"},
			Spec: manifest.HostKeySpec{
				HostID:    "openai-host",
				Account:   "acme-anthropic",
				ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "X"},
			},
		}},
	)
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindOwnerMismatch) {
		t.Fatalf("expected owner_mismatch, got:\n%s", Format(issues))
	}
}

func TestValidateGraph_PolicyMissingLinks(t *testing.T) {
	docs := fixture()
	docs = append(docs, manifest.Document{Policy: &manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "user-pol", Owner: manifest.WireOwner{Kind: "user"}},
		Spec: manifest.PolicySpec{
			Pools:      []string{"ghost-pool"},
			RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "ghost-rl"}},
			Budgets:    []manifest.PolicyBudgetDTO{{Ref: "ghost-budget"}},
		},
	}})
	issues := ValidateGraph(docs)
	for _, want := range [][2]string{{"Pool", "ghost-pool"}, {"RateLimit", "ghost-rl"}, {"Budget", "ghost-budget"}} {
		if !hasRefMissing(issues, want[0], want[1]) {
			t.Errorf("expected ref_missing → %s/%s, got:\n%s", want[0], want[1], Format(issues))
		}
	}
}

// A pool's keys must all authenticate to the pool's host.
func TestValidateGraph_PoolKeyAtAnotherHost(t *testing.T) {
	docs := append(fixture(),
		manifest.Document{Host: &manifest.HostDTO{
			Metadata: manifest.WireMeta{Name: "anthropic-host", Owner: manifest.WireOwner{Kind: "system"}},
		}},
		manifest.Document{HostKey: &manifest.HostKeyDTO{
			Metadata: manifest.WireMeta{Name: "openai-key"},
			Spec:     manifest.HostKeySpec{HostID: "openai-host", ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "X"}},
		}},
		manifest.Document{Pool: &manifest.PoolDTO{
			Metadata: manifest.WireMeta{Name: "mixed", Owner: manifest.WireOwner{Kind: "system"}},
			Spec:     manifest.PoolSpec{Host: "anthropic-host", Credentials: []string{"openai-key"}},
		}},
	)
	issues := ValidateGraph(docs)
	if !hasIssue(issues, KindOwnerMismatch) {
		t.Fatalf("expected owner_mismatch, got:\n%s", Format(issues))
	}
}

// A host-owned tier policy caps; it never hands out keys, and a scoped rate
// limit must name a model its host serves.
func TestValidateGraph_TierPolicyShape(t *testing.T) {
	tier := func(spec manifest.PolicySpec) []manifest.Document {
		return append(fixture(),
			manifest.Document{RateLimit: &manifest.RateLimitDTO{
				Metadata: manifest.WireMeta{Name: "tier-rl", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
			}},
			manifest.Document{HostKey: &manifest.HostKeyDTO{
				Metadata: manifest.WireMeta{Name: "openai-key"},
				Spec:     manifest.HostKeySpec{HostID: "openai-host", ValueFrom: manifest.HostKeyValueFrom{Kind: "env", Env: "X"}},
			}},
			manifest.Document{Policy: &manifest.PolicyDTO{
				Metadata: manifest.WireMeta{Name: "openai-tier-1", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
				Spec:     spec,
			}},
		)
	}
	for _, tc := range []struct {
		name string
		spec manifest.PolicySpec
		want string // "" = clean
	}{
		{"scoped to a served model", manifest.PolicySpec{RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "tier-rl", Models: []string{"openai/gpt-x"}}}}, ""},
		{"unscoped", manifest.PolicySpec{RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "tier-rl"}}}, ""},
		{"scoped to a model the host does not serve", manifest.PolicySpec{RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "tier-rl", Models: []string{"openai/gpt-unserved"}}}}, "matches no model host"},
		{"scoped to a host", manifest.PolicySpec{RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "tier-rl", Models: []string{"openai/gpt-x@other"}}}}, "names a host"},
		{"links pools", manifest.PolicySpec{Pools: []string{"openai-pool"}}, "links no pools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateGraph(tier(tc.spec))
			report := Format(issues)
			if tc.want == "" {
				if HasErrors(issues) {
					t.Fatalf("want clean, got:\n%s", report)
				}
				return
			}
			if !strings.Contains(report, tc.want) {
				t.Fatalf("report does not mention %q:\n%s", tc.want, report)
			}
		})
	}
}

func TestValidateGraph_PolicyBareModelName(t *testing.T) {
	cases := []struct{ entry, want string }{
		{"gpt-x", `"openai/gpt-x"`},
	}
	for _, tc := range cases {
		docs := append(fixture(), manifest.Document{Policy: &manifest.PolicyDTO{
			Metadata: manifest.WireMeta{Name: "bare-pol", Owner: manifest.WireOwner{Kind: "user"}},
			Spec:     manifest.PolicySpec{Models: []string{"openai", tc.entry}},
		}})
		issues := ValidateGraph(docs)
		var found bool
		for _, is := range issues {
			if is.Severity == SeverityError && is.Source.String() == "Policy/bare-pol.spec.models[1]" &&
				strings.Contains(is.Message, "bare model name") && strings.Contains(is.Message, tc.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: expected bare-model-name error suggesting %s, got:\n%s", tc.entry, tc.want, Format(issues))
		}
		if len(issues) != 1 {
			t.Fatalf("%s: expected exactly one issue, got:\n%s", tc.entry, Format(issues))
		}
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
