package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/policy"
)

// testResolver is a fixed-set Resolver for unit tests.
var testResolver = manifest.MapResolver{
	Providers:  map[string]string{"anthropic": "provider-aaa"},
	Hosts:      map[string]string{"anthropic-direct": "host-bbb", "bedrock-us-east": "host-ccc"},
	Policies:   map[string]string{"cheap-tier": "policy-ddd"},
	Models:     map[string]string{"openai/gpt-4o": "model-eee", "claude-3-5-sonnet": "model-fff"},
	HostKeys:   map[string]string{"openai-key-1": "hk-ggg", "bedrock-key-prod": "hk-hhh"},
	RateLimits: map[string]string{"cheap-tier-rpm": "rl-iii", "gpt-4o-tpm": "rl-jjj"},
	Pools:      map[string]string{"bedrock-claude": "pool-kkk"},
	Budgets:    map[string]string{"monthly": "budget-lll"},
	Accounts:   map[string]string{"acme-anthropic": "acc-mmm"},
}

var testRev = manifest.MapReverseResolver{
	Providers:  map[string]string{"provider-aaa": "anthropic"},
	Hosts:      map[string]string{"host-bbb": "anthropic-direct", "host-ccc": "bedrock-us-east"},
	Policies:   map[string]string{"policy-ddd": "cheap-tier"},
	Models:     map[string]string{"model-eee": "openai/gpt-4o", "model-fff": "claude-3-5-sonnet"},
	HostKeys:   map[string]string{"hk-ggg": "openai-key-1", "hk-hhh": "bedrock-key-prod"},
	RateLimits: map[string]string{"rl-iii": "cheap-tier-rpm", "rl-jjj": "gpt-4o-tpm"},
	Pools:      map[string]string{"pool-kkk": "bedrock-claude"},
	Budgets:    map[string]string{"budget-lll": "monthly"},
	Accounts:   map[string]string{"acc-mmm": "acme-anthropic"},
}

const policyYAML = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: Policy
metadata:
  name: cheap-tier
spec:
  models:
    - openai/gpt-4o
    - anthropic/claude-3-5-sonnet
  pools: [bedrock-claude]
  rateLimits:
    - ref: cheap-tier-rpm
    - ref: gpt-4o-tpm
      models: [openai/gpt-4o]
      shared: false
  budgets:
    - ref: monthly
`

func TestParse_Policy(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(policyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	if docs[0].Policy == nil {
		t.Fatal("want Policy doc, got nil")
	}
	p := docs[0].Policy
	if p.Metadata.Name != "cheap-tier" {
		t.Errorf("name: got %q", p.Metadata.Name)
	}
	if len(p.Spec.Models) != 2 {
		t.Errorf("models: want 2, got %d", len(p.Spec.Models))
	}
	if len(p.Spec.RateLimits) != 2 || p.Spec.RateLimits[1].Ref != "gpt-4o-tpm" {
		t.Errorf("rateLimits: got %+v", p.Spec.RateLimits)
	}
}

func TestToPolicy_HappyPath(t *testing.T) {
	docs, _ := manifest.Parse(strings.NewReader(policyYAML))
	pol, err := manifest.ToPolicy(*docs[0].Policy, testResolver)
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	if pol.Spec.Models[0] != "openai/gpt-4o" {
		t.Errorf("models[0]: want openai/gpt-4o (verbatim ref), got %q", pol.Spec.Models[0])
	}
	shared, each := true, false
	want := policy.Spec{
		Models:     []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet"},
		PoolIDs:    []string{"pool-kkk"},
		RateLimits: []policy.RateLimitLink{{RateLimitID: "rl-iii", Shared: &shared}, {RateLimitID: "rl-jjj", Models: []string{"openai/gpt-4o"}, Shared: &each}},
		Budgets:    []policy.BudgetLink{{BudgetID: "budget-lll", Shared: &shared}},
	}
	if !reflect.DeepEqual(pol.Spec, want) {
		t.Errorf("spec = %+v\nwant   %+v", pol.Spec, want)
	}
}

func TestToPolicy_MissingLink(t *testing.T) {
	docs, _ := manifest.Parse(strings.NewReader(policyYAML))
	for _, drop := range []string{"pools", "rateLimits", "budgets"} {
		r := testResolver
		switch drop {
		case "pools":
			r.Pools = nil
		case "rateLimits":
			r.RateLimits = nil
		case "budgets":
			r.Budgets = nil
		}
		var notFound *manifest.RefNotFoundError
		if _, err := manifest.ToPolicy(*docs[0].Policy, r); !errors.As(err, &notFound) {
			t.Errorf("unresolvable %s: err = %v, want RefNotFoundError", drop, err)
		}
	}
}

// Export then apply must not lose a link: every one renders by name and
// translates back to the same ids.
func TestFromPolicy_RoundTrip(t *testing.T) {
	docs, _ := manifest.Parse(strings.NewReader(policyYAML))
	pol, _ := manifest.ToPolicy(*docs[0].Policy, testResolver)
	dto := manifest.FromPolicy(pol, testRev)
	if dto.APIVersion != manifest.APIVersion {
		t.Errorf("apiVersion = %q, want %q", dto.APIVersion, manifest.APIVersion)
	}
	back, err := manifest.ToPolicy(dto, testResolver)
	if err != nil {
		t.Fatalf("ToPolicy of the export: %v", err)
	}
	if !reflect.DeepEqual(back.Spec, pol.Spec) {
		t.Errorf("round trip = %+v\nwant         %+v", back.Spec, pol.Spec)
	}
}

// A pre-v0.20 Policy is refused with a pointer to the new shape, and a
// v1alpha3 one still carrying a removed field is refused by name: ignoring
// either would drop a restriction the author meant.
func TestParse_RefusesThePreviousPolicyShape(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"old apiVersion", strings.Replace(policyYAML, manifest.APIVersion, manifest.PreviousAPIVersion, 1), "pre-v0.20 shape"},
		{"removed field", policyYAML + "  hostKeys: [openai-key-1]\n", "spec.hostKeys"},
		{"removed rate limit", policyYAML + "  rlBindings: []\n", "spec.rlBindings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manifest.Parse(strings.NewReader(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// Kinds v1alpha3 left unchanged still parse under the previous apiVersion.
func TestParse_AcceptsUnchangedKindsUnderThePreviousAPIVersion(t *testing.T) {
	doc := strings.Replace(rateLimitYAML, manifest.APIVersion, manifest.PreviousAPIVersion, 1)
	if _, err := manifest.Parse(strings.NewReader(doc)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

const multiDocYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: anthropic
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata:
  name: anthropic-direct
spec:
  baseURL: https://api.anthropic.com
`

func TestParse_MultiDoc(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(multiDocYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 docs, got %d", len(docs))
	}
	if docs[0].Provider == nil {
		t.Error("doc[0]: want Provider")
	}
	if docs[1].Host == nil {
		t.Error("doc[1]: want Host")
	}
}

func TestParse_UnknownAPIVersion(t *testing.T) {
	yaml := `apiVersion: other/v1
kind: Provider
metadata:
  name: foo
spec: {}
`
	_, err := manifest.Parse(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for unknown apiVersion")
	}
}

const modelYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: claude-3-5-sonnet
  owner:
    kind: provider
    id: anthropic
spec:
  snapshots:
    - name: claude-3-5-sonnet
      originalName: claude-3-5-sonnet-20241022
  pointer: claude-3-5-sonnet
`

func TestToModel_HappyPath(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(modelYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := manifest.ToModel(*docs[0].Model, testResolver)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if m.Meta.Owner.ID != "provider-aaa" {
		t.Errorf("owner.id: want provider-aaa, got %q", m.Meta.Owner.ID)
	}
	if len(m.Spec.Snapshots) != 1 || m.Spec.Snapshots[0].Name != "claude-3-5-sonnet" {
		t.Errorf("snapshots: want 1 snapshot 'claude-3-5-sonnet', got %+v", m.Spec.Snapshots)
	}
}

func TestModelProductLineRoundTrip(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(modelYAML + `  family: claude
  productLine: sonnet
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := manifest.ToModel(*docs[0].Model, testResolver)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if m.Spec.Family != "claude" || m.Spec.ProductLine != "sonnet" {
		t.Fatalf("parsed family/productLine = %q/%q, want claude/sonnet", m.Spec.Family, m.Spec.ProductLine)
	}
	if got := manifest.FromModel(m, testRev).Spec.ProductLine; got != "sonnet" {
		t.Errorf("rendered productLine = %q, want sonnet", got)
	}
}

func TestModelDeprecationReplacementResolvesByName(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(modelYAML + `  deprecation:
    status: deprecated
    replacement: openai/gpt-4o
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := *docs[0].Model
	m, err := manifest.ToModel(d, testResolver)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if got := m.Spec.Deprecation.Replacement; got != "model-eee" {
		t.Errorf("parsed replacement = %q, want model-eee", got)
	}
	if got := d.Spec.Deprecation.Replacement; got != "openai/gpt-4o" {
		t.Errorf("ToModel mutated the document: replacement = %q", got)
	}
	if got := manifest.FromModel(m, testRev).Spec.Deprecation.Replacement; got != "openai/gpt-4o" {
		t.Errorf("rendered replacement = %q, want openai/gpt-4o", got)
	}
	if m.Spec.Deprecation.Replacement != "model-eee" {
		t.Errorf("FromModel mutated the row")
	}

	d.Spec.Deprecation.Replacement = "no-such-model"
	_, err = manifest.ToModel(d, testResolver)
	var notFound *manifest.RefNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("unknown replacement: err = %v, want RefNotFoundError", err)
	}
}

const rateLimitYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: RateLimit
metadata:
  name: cheap-tier-rpm
spec:
  rules:
    - meter: requests
      amount: 100
      window: 1m
      strategy: token-bucket
`

func TestToRateLimit_HappyPath(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(rateLimitYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if docs[0].RateLimit == nil {
		t.Fatal("want RateLimit doc")
	}
	rl, err := manifest.ToRateLimit(*docs[0].RateLimit, testResolver)
	if err != nil {
		t.Fatalf("ToRateLimit: %v", err)
	}
	if len(rl.Spec.Rules) != 1 {
		t.Fatalf("rules: want 1, got %d", len(rl.Spec.Rules))
	}
	if rl.Spec.Rules[0].Amount != 100 {
		t.Errorf("amount: want 100, got %d", rl.Spec.Rules[0].Amount)
	}
}

const pricingYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Pricing
metadata:
  name: anthropic-direct-pricing
  owner:
    kind: host
    id: anthropic-direct
spec:
  currency: USD
  targetModels:
    - claude-3-5-sonnet
  rates:
    - meter: tokens.input
      unit: per_million
      amount: 3.00
    - meter: tokens.output
      unit: per_million
      amount: 15.00
    - meter: tokens.input
      unit: per_million
      amount: 6.00
      serviceTier: priority
`

func TestToPricing_HappyPath(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(pricingYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 1 || docs[0].Pricing == nil {
		t.Fatalf("want 1 Pricing doc")
	}
	p, err := manifest.ToPricing(*docs[0].Pricing, testResolver)
	if err != nil {
		t.Fatalf("ToPricing: %v", err)
	}
	if p.Meta.Owner.ID != "host-bbb" {
		t.Errorf("owner.id: want host-bbb, got %q", p.Meta.Owner.ID)
	}
	if p.Spec.Currency != "USD" {
		t.Errorf("currency: want USD, got %q", p.Spec.Currency)
	}
	if len(p.Spec.TargetModelIDs) != 1 || p.Spec.TargetModelIDs[0] != "model-fff" {
		t.Errorf("targetModelIDs: want [model-fff], got %v", p.Spec.TargetModelIDs)
	}
	if p.Spec.Rates[0].Meter != "tokens.input" {
		t.Errorf("rates[0].meter: want tokens.input, got %q", p.Spec.Rates[0].Meter)
	}
	if p.Spec.Rates[0].Amount != 3.00 {
		t.Errorf("rates[0].amount: want 3.00, got %f", p.Spec.Rates[0].Amount)
	}
	if p.Spec.Rates[2].ServiceTier != "priority" {
		t.Errorf("rates[2].serviceTier: want priority, got %q", p.Spec.Rates[2].ServiceTier)
	}
	if back := manifest.FromPricing(p, testRev); back.Spec.Rates[2].ServiceTier != "priority" {
		t.Errorf("FromPricing dropped serviceTier: %+v", back.Spec.Rates[2])
	}
}

func TestLoadDirSkipsDrafts(t *testing.T) {
	dir := t.TempDir()
	body := func(name string) string {
		return `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata: { name: ` + name + ` }
`
	}
	mustWrite := func(p, b string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dir, "providers", "live", "provider.yaml"), body("live"))
	mustWrite(filepath.Join(dir, "drafts", "providers", "drafted-dir", "provider.yaml"), body("drafted-dir"))
	mustWrite(filepath.Join(dir, "providers", "live", "extra.draft.yaml"), body("drafted-file"))

	docs, err := manifest.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1 (drafts/ + *.draft.yaml should be skipped)", len(docs))
	}
	if docs[0].Provider == nil || docs[0].Provider.Metadata.Name != "live" {
		t.Fatalf("expected the live provider, got %+v", docs[0])
	}
}
