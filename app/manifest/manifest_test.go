package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
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

// parseOne parses a manifest holding exactly one document.
func parseOne(t *testing.T, yaml string) manifest.Document {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil || len(docs) != 1 {
		t.Fatalf("Parse = %d docs, %v; want one", len(docs), err)
	}
	return docs[0]
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

// Model refs stay verbatim, links resolve to ids, and shared defaults to true; the export translates back to the same row.
func TestPolicy(t *testing.T) {
	doc := parseOne(t, policyYAML)
	if doc.Kind() != "Policy" || doc.Policy.Metadata.Name != "cheap-tier" {
		t.Fatalf("parsed a %s named %q, want the Policy cheap-tier", doc.Kind(), doc.Policy.Metadata.Name)
	}
	pol, err := manifest.ToPolicy(*doc.Policy, testResolver)
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	shared, each := true, false
	want := policy.Spec{
		Models:     []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet"},
		PoolIDs:    []string{"pool-kkk"},
		RateLimits: []policy.RateLimitLink{{RateLimitID: "rl-iii", Shared: &shared}, {RateLimitID: "rl-jjj", Models: []string{"openai/gpt-4o"}, Shared: &each}},
		Budgets:    []policy.BudgetLink{{BudgetID: "budget-lll", Shared: &shared}},
	}
	if diff := cmp.Diff(want, pol.Spec); diff != "" {
		t.Errorf("ToPolicy spec (-want +got):\n%s", diff)
	}
	dto := manifest.FromPolicy(pol, testRev)
	back, err := manifest.ToPolicy(dto, testResolver)
	if err != nil || dto.APIVersion != manifest.APIVersion || dto.Kind != "Policy" {
		t.Fatalf("export: apiVersion %q kind %q, ToPolicy of it: %v", dto.APIVersion, dto.Kind, err)
	}
	if diff := cmp.Diff(pol, back); diff != "" {
		t.Errorf("round trip (-row +back):\n%s", diff)
	}

	for _, tc := range []struct {
		name string
		drop func(r *manifest.MapResolver)
		want string
	}{
		{"pool", func(r *manifest.MapResolver) { r.Pools = nil }, `policy "cheap-tier": pool "bedrock-claude" not found`},
		{"rate limit", func(r *manifest.MapResolver) { r.RateLimits = map[string]string{"cheap-tier-rpm": "rl-iii"} }, `policy "cheap-tier": rateLimits[1] rate limit "gpt-4o-tpm" not found`},
		{"budget", func(r *manifest.MapResolver) { r.Budgets = nil }, `policy "cheap-tier": budgets[0] budget "monthly" not found`},
	} {
		r := testResolver
		tc.drop(&r)
		_, err := manifest.ToPolicy(*doc.Policy, r)
		var notFound *manifest.RefNotFoundError
		if got := errText(err); got != tc.want || !errors.As(err, &notFound) {
			t.Errorf("unresolvable %s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}

	// Model refs are stored in the canonical form the control API stores, or apply would report an update on every run against a row the API just normalised.
	mixed := manifest.PolicyDTO{
		Metadata: manifest.WireMeta{Name: "p", Owner: manifest.WireOwner{Kind: meta.OwnerSystem}},
		Spec: manifest.PolicySpec{
			Models:     []string{"OpenAI/GPT-4o", "openai/gpt-4o", "Anthropic/claude-3.5"},
			RateLimits: []manifest.PolicyRateLimitDTO{{Ref: "gpt-4o-tpm", Models: []string{"OpenAI/GPT-4o"}}},
		},
	}
	canon, err := manifest.ToPolicy(mixed, testResolver)
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	wantCanon := policy.Spec{
		Models:     []string{"openai/gpt-4o", "anthropic/claude-3-5"},
		RateLimits: []policy.RateLimitLink{{RateLimitID: "rl-jjj", Models: []string{"openai/gpt-4o"}, Shared: &shared}},
	}
	if diff := cmp.Diff(wantCanon, canon.Spec); diff != "" {
		t.Errorf("canonical model refs (-want +got):\n%s", diff)
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

// A pre-v0.20 Policy is refused with a pointer to the new shape, and a v1alpha3 one still carrying a removed field is refused by name: ignoring either would drop a restriction the author meant. Kinds v1alpha3 left unchanged still parse under the previous apiVersion.
func TestParseAPIVersions(t *testing.T) {
	previous := func(doc string) string {
		return strings.Replace(doc, manifest.APIVersion, manifest.PreviousAPIVersion, 1)
	}
	for _, tc := range []struct{ name, doc, want string }{
		{"a rate limit under the current apiVersion", strings.Replace(rateLimitYAML, manifest.PreviousAPIVersion, manifest.APIVersion, 1), ""},
		{"a rate limit under the previous apiVersion", rateLimitYAML, ""},
		{"a policy under the previous apiVersion", previous(policyYAML), "wire: doc 0: Policy under relay.wyolet.dev/v1alpha2 is the pre-v0.20 shape; rewrite it as relay.wyolet.dev/v1alpha3 (hostKeys, rateLimit, rlBindings and keySelection are gone: link pools or credentials, rateLimits: [{ref, models}] and budgets)"},
		{"a policy with a removed field", policyYAML + "  hostKeys: [openai-key-1]\n", "wire: doc 0 kind=Policy name=cheap-tier: Policy no longer has spec.hostKeys (use pools)"},
		{"a policy with a removed rate-limit field", policyYAML + "  rlBindings: []\n", "wire: doc 0 kind=Policy name=cheap-tier: Policy no longer has spec.rlBindings (use rateLimits: [{ref: <name>, models: [...]}])"},
		{"an unknown apiVersion", "apiVersion: other/v1\nkind: Provider\nmetadata:\n  name: foo\nspec: {}\n", `wire: doc 0: unsupported apiVersion "other/v1" (want "relay.wyolet.dev/v1alpha3")`},
		{"an unknown kind", "apiVersion: " + manifest.APIVersion + "\nkind: Widget\nmetadata:\n  name: foo\n", `wire: doc 0 kind=Widget name=foo: unknown kind "Widget"`},
	} {
		_, err := manifest.Parse(strings.NewReader(tc.doc))
		if got := errText(err); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestParseMultiDoc(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(`
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
`))
	if err != nil || len(docs) != 2 {
		t.Fatalf("Parse = %d docs, %v; want 2", len(docs), err)
	}
	if docs[0].Kind() != "Provider" || docs[0].Provider.Metadata.Name != "anthropic" || docs[1].Kind() != "Host" || docs[1].Host.Spec.BaseURL != "https://api.anthropic.com" {
		t.Errorf("docs = %s %+v, %s %+v; want the provider then the host", docs[0].Kind(), docs[0].Provider, docs[1].Kind(), docs[1].Host)
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

// The owner and a deprecation's replacement are names in the document and ids in the row; neither translation mutates its input.
func TestModel(t *testing.T) {
	d := *parseOne(t, modelYAML+"  family: claude\n  productLine: sonnet\n  deprecation:\n    status: deprecated\n    replacement: openai/gpt-4o\n").Model
	m, err := manifest.ToModel(d, testResolver)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	want := &model.Model{
		Meta: meta.Metadata{Name: "claude-3-5-sonnet", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "provider-aaa"}},
		Spec: model.Spec{
			Snapshots:   []model.Snapshot{{Name: "claude-3-5-sonnet", OriginalName: "claude-3-5-sonnet-20241022"}},
			Pointer:     "claude-3-5-sonnet",
			Family:      "claude",
			ProductLine: "sonnet",
			Deprecation: &model.Deprecation{Status: "deprecated", Replacement: "model-eee"},
		},
	}
	if diff := cmp.Diff(want, m); diff != "" {
		t.Errorf("ToModel (-want +got):\n%s", diff)
	}
	if d.Spec.Deprecation.Replacement != "openai/gpt-4o" || d.Metadata.Owner.ID != "anthropic" {
		t.Errorf("ToModel mutated the document: %+v", d)
	}
	back := manifest.FromModel(m, testRev)
	if diff := cmp.Diff(d.Spec, back.Spec); diff != "" || back.Metadata.Owner.Name != "anthropic" || back.Metadata.Owner.Kind != meta.OwnerProvider {
		t.Errorf("FromModel owner %+v, spec (-document +export):\n%s", back.Metadata.Owner, diff)
	}
	if m.Spec.Deprecation.Replacement != "model-eee" || m.Meta.Owner.ID != "provider-aaa" {
		t.Errorf("FromModel mutated the row: %+v", m)
	}

	d.Spec.Deprecation = &model.Deprecation{Status: "deprecated", Replacement: "no-such-model"}
	_, err = manifest.ToModel(d, testResolver)
	var notFound *manifest.RefNotFoundError
	if want := `model "claude-3-5-sonnet": deprecation.replacement: model "no-such-model" not found`; errText(err) != want || !errors.As(err, &notFound) {
		t.Errorf("an unknown replacement: err = %v, want RefNotFoundError %s", err, want)
	}
}

func TestToRateLimit(t *testing.T) {
	rl, err := manifest.ToRateLimit(*parseOne(t, rateLimitYAML).RateLimit, testResolver)
	if err != nil {
		t.Fatalf("ToRateLimit: %v", err)
	}
	want := []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 100, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}}
	if diff := cmp.Diff(want, rl.Spec.Rules); diff != "" || rl.Meta.Name != "cheap-tier-rpm" {
		t.Errorf("rate limit %q rules (-want +got):\n%s", rl.Meta.Name, diff)
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

func TestPricing(t *testing.T) {
	d := *parseOne(t, pricingYAML).Pricing
	p, err := manifest.ToPricing(d, testResolver)
	if err != nil {
		t.Fatalf("ToPricing: %v", err)
	}
	want := &pricing.Pricing{
		Meta: meta.Metadata{Name: "anthropic-direct-pricing", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "host-bbb"}},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{"model-fff"},
			Rates: []pricing.Rate{
				{Meter: "tokens.input", Unit: "per_million", Amount: 3},
				{Meter: "tokens.output", Unit: "per_million", Amount: 15},
				{Meter: "tokens.input", Unit: "per_million", Amount: 6, ServiceTier: "priority"},
			},
		},
	}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("ToPricing (-want +got):\n%s", diff)
	}
	back := manifest.FromPricing(p, testRev)
	if diff := cmp.Diff(d.Spec, back.Spec); diff != "" || back.Metadata.Owner.Name != "anthropic-direct" {
		t.Errorf("FromPricing owner %+v, spec (-document +export):\n%s", back.Metadata.Owner, diff)
	}
}

func TestLoadDirSkipsDrafts(t *testing.T) {
	dir := t.TempDir()
	for path, name := range map[string]string{
		"providers/live/provider.yaml":               "live",
		"drafts/providers/drafted-dir/provider.yaml": "drafted-dir",
		"providers/live/extra.draft.yaml":            "drafted-file",
		"providers/live/notes.txt":                   "not-yaml",
	} {
		p := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "apiVersion: relay.wyolet.dev/v1alpha2\nkind: Provider\nmetadata: { name: " + name + " }\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := manifest.LoadDir(dir)
	if err != nil || len(docs) != 1 || docs[0].Provider == nil || docs[0].Provider.Metadata.Name != "live" {
		t.Fatalf("LoadDir = %+v, %v; want only the live provider (drafts/, *.draft.yaml and non-YAML skipped)", docs, err)
	}
}
