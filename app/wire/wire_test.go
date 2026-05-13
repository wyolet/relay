package wire_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/wire"
)

// testResolver is a fixed-set Resolver for unit tests.
var testResolver = wire.MapResolver{
	Providers:  map[string]string{"anthropic": "provider-aaa"},
	Hosts:      map[string]string{"anthropic-direct": "host-bbb", "bedrock-us-east": "host-ccc"},
	Policies:   map[string]string{"cheap-tier": "policy-ddd"},
	Models:     map[string]string{"openai/gpt-4o": "model-eee", "claude-3-5-sonnet": "model-fff"},
	HostKeys:   map[string]string{"openai-key-1": "hk-ggg", "bedrock-key-prod": "hk-hhh"},
	RateLimits: map[string]string{"cheap-tier-rpm": "rl-iii"},
}

var testRev = wire.MapReverseResolver{
	Providers:  map[string]string{"provider-aaa": "anthropic"},
	Hosts:      map[string]string{"host-bbb": "anthropic-direct", "host-ccc": "bedrock-us-east"},
	Policies:   map[string]string{"policy-ddd": "cheap-tier"},
	Models:     map[string]string{"model-eee": "openai/gpt-4o", "model-fff": "claude-3-5-sonnet"},
	HostKeys:   map[string]string{"hk-ggg": "openai-key-1", "hk-hhh": "bedrock-key-prod"},
	RateLimits: map[string]string{"rl-iii": "cheap-tier-rpm"},
}

const policyYAML = `
apiVersion: relay.wyolet.dev/v1
kind: Policy
metadata:
  name: cheap-tier
spec:
  models:
    - openai/gpt-4o
    - claude-3-5-sonnet
  hostKeys:
    - openai-key-1
    - bedrock-key-prod
  rateLimit: cheap-tier-rpm
`

func TestParse_Policy(t *testing.T) {
	docs, err := wire.Parse(strings.NewReader(policyYAML))
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
	if p.Spec.RateLimit != "cheap-tier-rpm" {
		t.Errorf("rateLimit: got %q", p.Spec.RateLimit)
	}
}

func TestToPolicy_HappyPath(t *testing.T) {
	docs, _ := wire.Parse(strings.NewReader(policyYAML))
	pol, err := wire.ToPolicy(*docs[0].Policy, testResolver)
	if err != nil {
		t.Fatalf("ToPolicy: %v", err)
	}
	if len(pol.Spec.ModelIDs) != 2 {
		t.Errorf("modelIDs: want 2, got %d", len(pol.Spec.ModelIDs))
	}
	if pol.Spec.ModelIDs[0] != "model-eee" {
		t.Errorf("modelIDs[0]: want model-eee, got %q", pol.Spec.ModelIDs[0])
	}
	if pol.Spec.RateLimitID != "rl-iii" {
		t.Errorf("rateLimitID: want rl-iii, got %q", pol.Spec.RateLimitID)
	}
	if len(pol.Spec.HostKeyIDs) != 2 {
		t.Errorf("hostKeyIDs: want 2, got %d", len(pol.Spec.HostKeyIDs))
	}
}

func TestToPolicy_MissingRef(t *testing.T) {
	docs, _ := wire.Parse(strings.NewReader(policyYAML))
	emptyResolver := wire.MapResolver{
		Models:     map[string]string{},
		HostKeys:   map[string]string{},
		RateLimits: map[string]string{},
	}
	_, err := wire.ToPolicy(*docs[0].Policy, emptyResolver)
	if err == nil {
		t.Fatal("expected error for missing model ref, got nil")
	}
}

func TestFromPolicy_RoundTrip(t *testing.T) {
	docs, _ := wire.Parse(strings.NewReader(policyYAML))
	pol, _ := wire.ToPolicy(*docs[0].Policy, testResolver)
	dto := wire.FromPolicy(pol, testRev)
	if len(dto.Spec.Models) != 2 {
		t.Errorf("models: want 2, got %d", len(dto.Spec.Models))
	}
	if dto.Spec.Models[0] != "openai/gpt-4o" {
		t.Errorf("models[0]: want openai/gpt-4o, got %q", dto.Spec.Models[0])
	}
	if dto.Spec.RateLimit != "cheap-tier-rpm" {
		t.Errorf("rateLimit: want cheap-tier-rpm, got %q", dto.Spec.RateLimit)
	}
}

const multiDocYAML = `
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: anthropic
spec: {}
---
apiVersion: relay.wyolet.dev/v1
kind: Host
metadata:
  name: anthropic-direct
spec:
  baseURL: https://api.anthropic.com
`

func TestParse_MultiDoc(t *testing.T) {
	docs, err := wire.Parse(strings.NewReader(multiDocYAML))
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
	_, err := wire.Parse(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected error for unknown apiVersion")
	}
}

const modelYAML = `
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: claude-3-5-sonnet
  owner:
    kind: provider
    id: anthropic
spec:
  hosts:
    - host: anthropic-direct
      upstreamName: claude-3-5-sonnet-20241022
`

func TestToModel_HappyPath(t *testing.T) {
	docs, err := wire.Parse(strings.NewReader(modelYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := wire.ToModel(*docs[0].Model, testResolver)
	if err != nil {
		t.Fatalf("ToModel: %v", err)
	}
	if len(m.Spec.Hosts) != 1 {
		t.Fatalf("hosts: want 1, got %d", len(m.Spec.Hosts))
	}
	if m.Spec.Hosts[0].HostID != "host-bbb" {
		t.Errorf("hostID: want host-bbb, got %q", m.Spec.Hosts[0].HostID)
	}
	if m.Meta.Owner.ID != "provider-aaa" {
		t.Errorf("owner.id: want provider-aaa, got %q", m.Meta.Owner.ID)
	}
}

const rateLimitYAML = `
apiVersion: relay.wyolet.dev/v1
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
	docs, err := wire.Parse(strings.NewReader(rateLimitYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if docs[0].RateLimit == nil {
		t.Fatal("want RateLimit doc")
	}
	rl, err := wire.ToRateLimit(*docs[0].RateLimit, testResolver)
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
