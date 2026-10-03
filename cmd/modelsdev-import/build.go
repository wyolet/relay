package main

import (
	"fmt"
	"sort"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

const apiVersion = "relay.wyolet.dev/v1alpha2"

func boolPtr(b bool) *bool { return &b }

func buildProvider(p MDProvider) manifest.ProviderDTO {
	return manifest.ProviderDTO{
		APIVersion: apiVersion,
		Kind:       "Provider",
		Metadata: manifest.WireMeta{
			Name:        p.ID,
			DisplayName: p.Name,
			Labels:      map[string]string{"source": "models.dev"},
		},
		Spec: manifest.ProviderSpec{DocsURL: p.Doc},
	}
}

func buildHost(p MDProvider, baseURL string) manifest.HostDTO {
	return manifest.HostDTO{
		APIVersion: apiVersion,
		Kind:       "Host",
		Metadata: manifest.WireMeta{
			Name:        p.ID,
			DisplayName: p.Name,
			Labels:      map[string]string{"source": "models.dev"},
		},
		Spec: manifest.HostSpec{
			BaseURL:           baseURL,
			DocsURL:           p.Doc,
			PricingStrategies: pricingStrategiesFor(p.ID),
		},
	}
}

// buildFoldedModel builds one Model from a fold group. base is the model
// name/pointer; pm is the primary member (source of capabilities/context/
// displayName); members are all models.dev ids in the group, each becoming a
// Snapshot (verbatim key → OriginalName when it isn't slug-clean; release_date
// → releasedAt). This matches the hand-curated convention of one Model with
// dated snapshots rather than one model per dated id.
func buildFoldedModel(p MDProvider, pm MDModel, base string, members []string, version string) manifest.ModelDTO {
	labels := map[string]string{"source": "models.dev"}
	if version != "" {
		labels["source_version"] = version
	}
	if pm.Family != "" {
		labels["family"] = pm.Family
	}

	snaps := make([]model.Snapshot, 0, len(members))
	for _, mid := range members {
		s := model.Snapshot{Name: slugify(mid)}
		if mid != s.Name {
			s.OriginalName = mid // verbatim wire name
		}
		if rd := p.Models[mid].ReleaseDate; rd != "" {
			s.ReleasedAt = rd
		}
		snaps = append(snaps, s)
	}

	spec := manifest.ModelSpec{
		Family:          pm.Family,
		Capabilities:    capabilities(pm),
		Modalities:      model.Modalities{Input: pm.Modalities.Input, Output: pm.Modalities.Output},
		KnowledgeCutoff: pm.Knowledge,
		ReleaseDate:     pm.ReleaseDate,
		Enabled:         boolPtr(true),
		Snapshots:       snaps,
		Pointer:         slugify(pm.ID),
	}
	spec.ContextWindowTotal = pm.Limit.Context
	if pm.Limit.Input > 0 {
		spec.ContextWindowInput = pm.Limit.Input
	} else {
		spec.ContextWindowInput = pm.Limit.Context
	}
	spec.ContextWindowOutput = pm.Limit.Output
	spec.MaxOutputTokens = pm.Limit.Output

	if pm.OpenWeights {
		spec.Tags = []string{"open-weights"}
	}

	return manifest.ModelDTO{
		APIVersion: apiVersion,
		Kind:       "Model",
		Metadata: manifest.WireMeta{
			Name:        base,
			DisplayName: pm.Name,
			Owner:       manifest.WireOwner{Kind: meta.OwnerKind("provider"), Name: p.ID},
			Labels:      labels,
		},
		Spec: spec,
	}
}

func capabilities(m MDModel) model.Capabilities {
	c := model.Capabilities{
		Chat:           true,
		Streaming:      true,
		SystemMessages: true,
		Tools:          m.ToolCall,
		Reasoning:      m.Reasoning,
	}
	for _, in := range m.Modalities.Input {
		switch in {
		case "image":
			c.Vision = true
		case "pdf", "file":
			c.FileInput = true
		case "audio":
			c.Audio = true
			c.AudioInput = true
		}
	}
	for _, out := range m.Modalities.Output {
		if out == "audio" {
			c.AudioOutput = true
		}
	}
	if m.Cost.CacheRead != nil || m.Cost.CacheWrite != nil {
		c.PromptCache = true
	}
	if m.Temperature != nil && !*m.Temperature {
		c.UnsupportedParams = []string{"temperature"}
	}
	return c
}

// buildFoldedBinding emits one binding for the folded model. It lists every
// member's snapshot name; the per-snapshot wire names live on the model's
// Snapshot.OriginalName, so no binding-level UpstreamName is needed.
func buildFoldedBinding(base, hostID, adapter, primary string, members []string) manifest.HostBindingDTO {
	snaps := make([]string, 0, len(members))
	for _, mid := range members {
		snaps = append(snaps, slugify(mid))
	}
	return manifest.HostBindingDTO{
		APIVersion: apiVersion,
		Kind:       "HostBinding",
		Metadata: manifest.WireMeta{
			Name:        base + "-on-" + hostID,
			DisplayName: fmt.Sprintf("%s via %s", base, hostID),
		},
		Spec: manifest.HostBindingSpec{
			Model:     base,
			Host:      hostID,
			Adapter:   adapter,
			Enabled:   boolPtr(true),
			Snapshots: snaps,
		},
	}
}

// buildPricing turns a models.dev cost block into a Pricing DTO. Base rates
// carry aboveTokens=0; volume tiers add rates at their context-size
// threshold. Returns ok=false when the model has no priced meters.
func buildPricing(hostID, slug string, cost MDCost) (manifest.PricingDTO, bool) {
	var rates []manifest.PricingRateDTO
	add := func(key string, amt *float64, above int) {
		if amt == nil {
			return
		}
		meter, ok := meterFor[key]
		if !ok {
			return
		}
		rates = append(rates, manifest.PricingRateDTO{
			Meter:       meter,
			Unit:        "per_million",
			Amount:      *amt,
			AboveTokens: above,
		})
	}

	add("input", cost.Input, 0)
	add("output", cost.Output, 0)
	add("cache_read", cost.CacheRead, 0)
	add("cache_write", cost.CacheWrite, 0)
	add("reasoning", cost.Reasoning, 0)
	add("input_audio", cost.InputAudio, 0)
	add("output_audio", cost.OutputAudio, 0)

	for _, t := range cost.Tiers {
		above := t.Tier.Size
		if above == 0 {
			above = 200_000
		}
		add("input", t.Input, above)
		add("output", t.Output, above)
		add("cache_read", t.CacheRead, above)
		add("cache_write", t.CacheWrite, above)
	}
	if len(cost.Tiers) == 0 && cost.ContextOver200k != nil {
		o := cost.ContextOver200k
		add("input", o.Input, 200_000)
		add("output", o.Output, 200_000)
		add("cache_read", o.CacheRead, 200_000)
		add("cache_write", o.CacheWrite, 200_000)
	}

	if len(rates) == 0 {
		return manifest.PricingDTO{}, false
	}
	sort.SliceStable(rates, func(i, j int) bool {
		if rates[i].Meter != rates[j].Meter {
			return rates[i].Meter < rates[j].Meter
		}
		return rates[i].AboveTokens < rates[j].AboveTokens
	})

	return manifest.PricingDTO{
		APIVersion: apiVersion,
		Kind:       "Pricing",
		Metadata: manifest.WireMeta{
			Name:        hostID + "-" + slug,
			DisplayName: slug + " pricing",
			Owner:       manifest.WireOwner{Kind: meta.OwnerKind("host"), ID: hostID},
		},
		Spec: manifest.PricingSpec{
			Currency:     "USD",
			TargetModels: []string{slug},
			Rates:        rates,
			Enabled:      boolPtr(true),
		},
	}, true
}
