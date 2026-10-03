package main

import (
	"github.com/wyolet/relay/app/manifest"
)

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
