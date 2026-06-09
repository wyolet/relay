package main

import (
	"testing"
)

func fp(v float64) *float64 { return &v }

// knownMeters mirrors the meters relay's cost engine understands
// (sdk/catalog cost.go meterForUsageKey + app/pricing). A generated meter
// outside this set computes to $0 silently — the closure test guards it.
var knownMeters = map[string]bool{
	"tokens.input": true, "tokens.output": true,
	"tokens.cache_read": true, "tokens.cache_creation": true,
	"tokens.reasoning": true, "tokens.audio_input": true, "tokens.audio_output": true,
	"tokens.accepted_prediction": true, "tokens.rejected_prediction": true,
	"tokens.server_tool_use_input": true, "tokens.server_tool_use_output": true,
}

func prov(id, npm string, models map[string]MDModel) MDProvider {
	return MDProvider{ID: id, Name: id, NPM: npm, API: "https://" + id + ".example.com", Models: models}
}

// TestMetersAreKnown — every emitted pricing meter is one relay understands.
func TestMetersAreKnown(t *testing.T) {
	m := MDModel{ID: "m1", Name: "M1"}
	m.Cost = MDCost{
		Input: fp(3), Output: fp(15), CacheRead: fp(0.3), CacheWrite: fp(3.75),
		Reasoning: fp(1), InputAudio: fp(2), OutputAudio: fp(4),
		Tiers: []MDTier{{MDTierRates: MDTierRates{Input: fp(6), Output: fp(30)}, Tier: struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		}{Type: "context", Size: 200000}}},
	}
	r, _ := Translate([]MDProvider{prov("p", "@ai-sdk/openai-compatible", map[string]MDModel{"m1": m})}, Opts{})
	if len(r.Pricings) == 0 {
		t.Fatal("no pricing emitted")
	}
	for _, pr := range r.Pricings {
		for _, rate := range pr.Spec.Rates {
			if !knownMeters[rate.Meter] {
				t.Errorf("emitted unknown meter %q (would compute to $0)", rate.Meter)
			}
		}
	}
	// cache_write must have mapped to cache_creation, not passed through.
	for _, pr := range r.Pricings {
		for _, rate := range pr.Spec.Rates {
			if rate.Meter == "cache_write" || rate.Meter == "tokens.cache_write" {
				t.Error("cache_write leaked unmapped")
			}
		}
	}
}

// TestSnapshotFolding — dated/alias variants fold into one Model with snapshots.
func TestSnapshotFolding(t *testing.T) {
	models := map[string]MDModel{
		"opus":          {ID: "opus", ReleaseDate: "2026-01-01"},
		"opus-20260101": {ID: "opus-20260101", ReleaseDate: "2026-01-01"},
		"opus-latest":   {ID: "opus-latest"},
	}
	r, _ := Translate([]MDProvider{prov("anthropic", "@ai-sdk/anthropic", models)}, Opts{})
	if len(r.Models) != 1 {
		t.Fatalf("want 1 folded model, got %d", len(r.Models))
	}
	if got := r.Models[0].Metadata.Name; got != "opus" {
		t.Errorf("model name = %q, want opus", got)
	}
	if n := len(r.Models[0].Spec.Snapshots); n != 3 {
		t.Errorf("want 3 snapshots, got %d", n)
	}
	if r.Models[0].Spec.Pointer == "" {
		t.Error("pointer not set")
	}
	// the single binding must reference all 3 snapshots, all declared on the model.
	if len(r.Bindings) != 1 {
		t.Fatalf("want 1 binding, got %d", len(r.Bindings))
	}
	assertBindingSnapshotsDeclared(t, r)
}

// TestCrossHostDedupUnionsSnapshots — the data-loss guard. One model served by
// two hosts with different snapshot sets becomes ONE Model whose snapshots are
// the union, two bindings, and every binding references only declared snapshots.
func TestCrossHostDedupUnionsSnapshots(t *testing.T) {
	a := prov("hosta", "@ai-sdk/openai-compatible", map[string]MDModel{
		"glm":          {ID: "glm"},
		"glm-20260101": {ID: "glm-20260101"},
	})
	b := prov("hostb", "@ai-sdk/openai-compatible", map[string]MDModel{
		"glm":          {ID: "glm"},
		"glm-20260202": {ID: "glm-20260202"},
	})
	r, _ := Translate([]MDProvider{a, b}, Opts{})

	glm := 0
	for _, m := range r.Models {
		if m.Metadata.Name == "glm" {
			glm++
			names := map[string]bool{}
			for _, s := range m.Spec.Snapshots {
				names[s.Name] = true
			}
			for _, want := range []string{"glm", "glm-20260101", "glm-20260202"} {
				if !names[want] {
					t.Errorf("union missing snapshot %q (data loss)", want)
				}
			}
		}
	}
	if glm != 1 {
		t.Fatalf("want exactly 1 deduped glm model, got %d", glm)
	}
	if len(r.Bindings) != 2 {
		t.Errorf("want 2 bindings (one per host), got %d", len(r.Bindings))
	}
	assertBindingSnapshotsDeclared(t, r) // no snapshot_missing by construction
}

// TestAdditiveSkip — a model whose name is already in the catalog is skipped.
func TestAdditiveSkip(t *testing.T) {
	r, _ := Translate(
		[]MDProvider{prov("p", "@ai-sdk/openai-compatible", map[string]MDModel{"keep": {ID: "keep"}, "skip": {ID: "skip"}})},
		Opts{Existing: map[string]bool{"skip": true}},
	)
	for _, m := range r.Models {
		if m.Metadata.Name == "skip" {
			t.Error("additive: skip model should not be emitted")
		}
	}
	if r.SkippedExisting != 1 {
		t.Errorf("SkippedExisting = %d, want 1", r.SkippedExisting)
	}
}

// TestUpstreamVerbatim — a non-slug-clean key is preserved as snapshot OriginalName.
func TestUpstreamVerbatim(t *testing.T) {
	r, _ := Translate([]MDProvider{prov("minimax", "@ai-sdk/anthropic", map[string]MDModel{
		"MiniMax-M2.1": {ID: "MiniMax-M2.1"},
	})}, Opts{})
	if len(r.Models) != 1 {
		t.Fatalf("want 1 model, got %d", len(r.Models))
	}
	s := r.Models[0].Spec.Snapshots
	if len(s) != 1 || s[0].OriginalName != "MiniMax-M2.1" {
		t.Errorf("verbatim upstream not preserved: %+v", s)
	}
	if r.Models[0].Metadata.Name != "minimax-m2-1" {
		t.Errorf("slug = %q, want minimax-m2-1", r.Models[0].Metadata.Name)
	}
	// minimax → anthropic adapter from the @ai-sdk tag.
	if r.Bindings[0].Spec.Adapter != "anthropic" {
		t.Errorf("adapter = %q, want anthropic", r.Bindings[0].Spec.Adapter)
	}
}

// assertBindingSnapshotsDeclared verifies every binding's snapshots appear in
// the referenced model's snapshot list — the invariant catalog-validate's
// snapshot_missing check enforces.
func assertBindingSnapshotsDeclared(t *testing.T, r *TranslateResult) {
	t.Helper()
	declared := map[string]map[string]bool{}
	for _, m := range r.Models {
		s := map[string]bool{}
		for _, sn := range m.Spec.Snapshots {
			s[sn.Name] = true
		}
		declared[m.Metadata.Name] = s
	}
	for _, b := range r.Bindings {
		for _, sn := range b.Spec.Snapshots {
			if !declared[b.Spec.Model][sn] {
				t.Errorf("binding %q references undeclared snapshot %q on model %q",
					b.Metadata.Name, sn, b.Spec.Model)
			}
		}
	}
}
