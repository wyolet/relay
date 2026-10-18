package curation

import (
	"testing"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

func TestRuleNames(t *testing.T) {
	var got []string
	for _, r := range catalogvalidate.ListRules(Rules) {
		got = append(got, r.Name)
	}
	want := []string{"host-has-tier-policy", "pricing-target-must-bind-to-host", "provider-icon-set"}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rules = %v, want %v", got, want)
		}
	}
}

func TestPricingTargetHostBinding_OK(t *testing.T) {
	docs := []manifest.Document{
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "openai-host"}}},
		{Model: &manifest.ModelDTO{
			Metadata: manifest.WireMeta{Name: "gpt-x"},
			Spec: manifest.ModelSpec{
				Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}},
				Pointer:   "gpt-x",
			},
		}},
		{HostBinding: &manifest.HostBindingDTO{
			Metadata: manifest.WireMeta{Name: "gpt-x@openai-host"},
			Spec:     manifest.HostBindingSpec{Model: "gpt-x", Host: "openai-host"},
		}},
		{Pricing: &manifest.PricingDTO{
			Metadata: manifest.WireMeta{Name: "p1", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
			Spec:     manifest.PricingSpec{Currency: "USD", TargetModels: []string{"gpt-x"}},
		}},
	}
	got := checkPricingTargetHostBinding(docs)
	if len(got) != 0 {
		t.Fatalf("expected no issues, got: %v", got)
	}
}

func TestPricingTargetHostBinding_BindingMissing(t *testing.T) {
	docs := []manifest.Document{
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "openai-host"}}},
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "anthropic-host"}}},
		{Model: &manifest.ModelDTO{
			Metadata: manifest.WireMeta{Name: "gpt-x"},
			Spec: manifest.ModelSpec{
				Snapshots: []model.Snapshot{{Name: "gpt-x", OriginalName: "gpt-x"}},
				Pointer:   "gpt-x",
			},
		}},
		{HostBinding: &manifest.HostBindingDTO{
			// Bound to anthropic-host, not openai-host.
			Metadata: manifest.WireMeta{Name: "gpt-x@anthropic-host"},
			Spec:     manifest.HostBindingSpec{Model: "gpt-x", Host: "anthropic-host"},
		}},
		{Pricing: &manifest.PricingDTO{
			Metadata: manifest.WireMeta{Name: "p1", Owner: manifest.WireOwner{Kind: "host", Name: "openai-host"}},
			Spec:     manifest.PricingSpec{Currency: "USD", TargetModels: []string{"gpt-x"}},
		}},
	}
	got := checkPricingTargetHostBinding(docs)
	if len(got) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(got), got)
	}
	if got[0].Severity != catalogvalidate.SeverityError {
		t.Fatalf("expected error severity, got %v", got[0].Severity)
	}
}

func TestProviderIcon_MissingIsWarning(t *testing.T) {
	docs := []manifest.Document{
		{Provider: &manifest.ProviderDTO{
			Metadata: manifest.WireMeta{Name: "no-icon"},
			Spec:     manifest.ProviderSpec{},
		}},
	}
	got := checkProviderIcon(docs)
	if len(got) != 1 || got[0].Severity != catalogvalidate.SeverityWarning {
		t.Fatalf("expected 1 warning, got: %v", got)
	}
}

func TestProviderIcon_PresentIsOK(t *testing.T) {
	docs := []manifest.Document{
		{Provider: &manifest.ProviderDTO{
			Metadata: manifest.WireMeta{Name: "ok"},
			Spec:     manifest.ProviderSpec{Icon: &meta.Icon{Path: "/provider/ok.png"}},
		}},
	}
	got := checkProviderIcon(docs)
	if len(got) != 0 {
		t.Fatalf("expected no issues, got: %v", got)
	}
}

func TestHostHasTierPolicy_OK(t *testing.T) {
	docs := []manifest.Document{
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "h"}}},
		{Policy: &manifest.PolicyDTO{
			Metadata: manifest.WireMeta{Name: "h-tier-1", Owner: manifest.WireOwner{Kind: "host", Name: "h"}},
		}},
	}
	got := checkHostHasTierPolicy(docs)
	if len(got) != 0 {
		t.Fatalf("expected no issues, got: %v", got)
	}
}

func TestHostHasTierPolicy_NoPolicyWarns(t *testing.T) {
	docs := []manifest.Document{
		{Host: &manifest.HostDTO{Metadata: manifest.WireMeta{Name: "lonely"}}},
	}
	got := checkHostHasTierPolicy(docs)
	if len(got) != 1 || got[0].Severity != catalogvalidate.SeverityWarning {
		t.Fatalf("expected 1 warning, got: %v", got)
	}
}
