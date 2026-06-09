package main

import (
	"fmt"
	"sort"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

const apiVersion = "relay.wyolet.dev/v1alpha2"

// TranslateResult is the full set of catalog DTOs produced from models.dev,
// partitioned into shipped vs draft (unsupported adapter / no baseURL).
type TranslateResult struct {
	Providers []manifest.ProviderDTO
	Hosts     []manifest.HostDTO
	Models    []manifest.ModelDTO
	Bindings  []manifest.HostBindingDTO
	Pricings  []manifest.PricingDTO

	// Draft holds the same kinds for entries that must not ship yet.
	Draft DraftSet

	SkippedNoBaseURL int
	SkippedExisting  int            // folded models skipped because already in catalog (additive)
	UnsupportedNPM   map[string]int // npm tag → count routed to drafts
}

// DraftSet mirrors the shipped partition for drafts/.
type DraftSet struct {
	Providers []manifest.ProviderDTO
	Hosts     []manifest.HostDTO
	Models    []manifest.ModelDTO
	Bindings  []manifest.HostBindingDTO
	Pricings  []manifest.PricingDTO
}

// Opts controls Translate's behavior.
type Opts struct {
	Allow      map[string]bool // provider ids to ship (empty = all)
	Version    string          // source_version stamp
	DraftAll   bool            // route ALL output to drafts/, not just unsupported
	ProcessAll bool            // process every provider regardless of Allow (Allow then only tags the supported set)
	// Existing is the set of model names already present in the target
	// catalog. Folded models whose base name is in this set are skipped
	// entirely (additive import: never touch existing curated/imported
	// entries — refresh is the watcher's surgical, reviewed job).
	Existing map[string]bool
}

// Translate converts allowlisted models.dev providers into catalog DTOs.
// Dated/alias variants of one model (claude-haiku-4-5 + claude-haiku-4-5-
// 20251001 + ...-latest) fold into a single Model with multiple Snapshots —
// the catalog convention — rather than one model per models.dev id.
func Translate(providers []MDProvider, o Opts) (*TranslateResult, error) {
	r := &TranslateResult{UnsupportedNPM: map[string]int{}}

	// Accumulate models by base slug so a model served by multiple hosts
	// becomes ONE Model whose snapshots are the UNION across hosts — every
	// host's binding then references only snapshots the Model declares (no
	// snapshot_missing, no dropped snapshots). Bindings/pricings are per-host
	// and appended live. Models flatten into r at the end, preserving order.
	type modelAcc struct {
		dto   *manifest.ModelDTO
		draft bool
		seen  map[string]bool // snapshot names already on dto
	}
	acc := map[string]*modelAcc{}
	var accOrder []string
	providerOwnsModel := map[string]bool{} // provider id → owns ≥1 (first-seen) model
	addModel := func(base string, dto manifest.ModelDTO, draft bool, owner string) {
		if a, ok := acc[base]; ok {
			for _, s := range dto.Spec.Snapshots {
				if !a.seen[s.Name] {
					a.seen[s.Name] = true
					a.dto.Spec.Snapshots = append(a.dto.Spec.Snapshots, s)
				}
			}
			return
		}
		cp := dto
		a := &modelAcc{dto: &cp, draft: draft, seen: map[string]bool{}}
		for _, s := range cp.Spec.Snapshots {
			a.seen[s.Name] = true
		}
		acc[base] = a
		accOrder = append(accOrder, base)
		providerOwnsModel[owner] = true
	}

	// Providers are stashed and emitted at the end — only those that own a
	// model. A provider whose models all dedup into another provider's Model
	// (e.g. zhipuai's GLM folding into zai) would otherwise be an orphan. Its
	// Host still emits eagerly below (bindings need it).
	type provStash struct {
		dto   manifest.ProviderDTO
		draft bool
	}
	var provStashes []provStash

	for _, p := range providers {
		if !o.ProcessAll && len(o.Allow) > 0 && !o.Allow[p.ID] {
			continue
		}
		baseURL, ok := baseURLFor(p)
		if !ok {
			r.SkippedNoBaseURL++
			continue
		}
		adapter, supported := adapterForNPM(p.NPM)
		draft := !supported || o.DraftAll
		if !supported {
			r.UnsupportedNPM[p.NPM]++
		}

		provStashes = append(provStashes, provStash{dto: buildProvider(p), draft: draft})
		hostDTO := buildHost(p, baseURL)
		if draft {
			r.Draft.Hosts = append(r.Draft.Hosts, hostDTO)
		} else {
			r.Hosts = append(r.Hosts, hostDTO)
		}

		// Group this provider's models by fold key (base slug).
		groups := map[string][]string{}
		var order []string
		for _, mid := range sortedModelIDs(p) {
			slug := slugify(mid)
			if slug == "" {
				continue
			}
			fk := foldKey(slug)
			if _, seen := groups[fk]; !seen {
				order = append(order, fk)
			}
			groups[fk] = append(groups[fk], mid)
		}

		for _, base := range order {
			members := groups[base]
			if o.Existing[base] {
				r.SkippedExisting++ // additive: leave curated/imported entries alone
				continue
			}
			primary := choosePrimary(members, base)
			pm := p.Models[primary]

			modelDTO := buildFoldedModel(p, pm, base, members, o.Version)
			bindingDTO := buildFoldedBinding(base, p.ID, adapter, primary, members)
			pricingDTO, hasPricing := buildPricing(p.ID, base, pm.Cost)

			addModel(base, modelDTO, draft, p.ID)
			if draft {
				r.Draft.Bindings = append(r.Draft.Bindings, bindingDTO)
				if hasPricing {
					r.Draft.Pricings = append(r.Draft.Pricings, pricingDTO)
				}
			} else {
				r.Bindings = append(r.Bindings, bindingDTO)
				if hasPricing {
					r.Pricings = append(r.Pricings, pricingDTO)
				}
			}
		}
	}

	// Flatten accumulated models into the result, preserving first-seen order.
	for _, base := range accOrder {
		a := acc[base]
		if a.draft {
			r.Draft.Models = append(r.Draft.Models, *a.dto)
		} else {
			r.Models = append(r.Models, *a.dto)
		}
	}
	// Emit only providers that own a model (prune deduped-away orphans).
	for _, ps := range provStashes {
		if !providerOwnsModel[ps.dto.Metadata.Name] {
			continue
		}
		if ps.draft {
			r.Draft.Providers = append(r.Draft.Providers, ps.dto)
		} else {
			r.Providers = append(r.Providers, ps.dto)
		}
	}
	return r, nil
}

// choosePrimary picks the canonical member of a fold group: the bare base
// slug if present, else the "-latest" alias, else the last (most recent,
// since ids sort ascending).
func choosePrimary(members []string, base string) string {
	for _, m := range members {
		if slugify(m) == base {
			return m
		}
	}
	for _, m := range members {
		if slugify(m) == base+"-latest" {
			return m
		}
	}
	return members[len(members)-1]
}

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
