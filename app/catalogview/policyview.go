package catalogview

import (
	"sort"

	"context"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
)

// policyview.go is the policy-detail mirror of the model-detail projections:
// resource-navigation views for a single policy. A policy is a hub, so its
// detail page navigates three ways:
//
//   - models      — the models this policy grants, with the limits it applies
//                    to each (the inverse of ModelPolicies, same grant rules).
//   - hosts       — the hosts this policy can reach, each with the host-keys
//                    that reach it (host-keys are folded into hosts, never a
//                    surface of their own; credential values never exposed).
//   - rate-limits — the rate-limit rule sets the policy references: each
//                    RLBinding (per-model) plus the flat default, resolved to
//                    limits.

// PolicyRef identifies the policy a sub-resource response belongs to.
type PolicyRef struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Owner       OwnerRef `json:"owner"`
	Enabled     bool     `json:"enabled"`
}

type HostKeyRef struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Enabled               bool   `json:"enabled"`
	SharedWithPolicyCount int    `json:"sharedWithPolicyCount"` // # of OTHER policies referencing this key
}

// PolicyBindingRow — one concrete (provider, model, host) binding this policy
// grants, fully pre-joined. Mirrors ModelHostRow: the frontend renders rows
// and groups by host client-side, no catalog round-trip. MatchedBy carries the
// grant ref(s) that produced this row (raw Spec.Models entries, "*" for the
// implicit wildcard, or the model's canonical ref for an explicit ModelIDs
// grant) — the one fact the client can't recompute. Limits are resolved for
// THIS exact triple.
type PolicyBindingRow struct {
	Provider  ProviderRef `json:"provider"`
	Host      HostRef     `json:"host"`
	Model     ModelRef    `json:"model"`
	Binding   BindingView `json:"binding"`
	MatchedBy []string    `json:"matchedBy"`
	Limits    []Limit     `json:"limits"`
}

// PolicyModelExclusion — a model this policy does NOT grant, with why. Surfaced
// by PolicyModelExclusions (the ?debug view) to make an empty grant diagnosable
// — e.g. a host-tier policy whose host has no bindings post-migration.
type PolicyModelExclusion struct {
	Model  ModelRef `json:"model"`
	Reason string   `json:"reason"`
}

// PolicyHostRow — a host this policy can reach, with the host-keys that reach
// it (empty for a host-tier policy's own host, which needs no key to qualify).
// Requirement is "required" when this host uniquely serves at least one model
// the policy grants (losing its key would drop that model), else "optional" —
// a sibling reachable host covers everything it does.
type PolicyHostRow struct {
	Host        HostRef      `json:"host"`
	HostKeys    []HostKeyRef `json:"hostKeys"`
	Requirement string       `json:"requirement"` // "required" | "optional"
}

// PolicyRateLimitRow — one rate-limit rule set the policy references. Default
// is the flat Spec.RateLimitID; the others are per-model RLBindings carrying
// their Models DSL.
type PolicyRateLimitRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Default bool     `json:"default"`
	Models  []string `json:"models,omitempty"`
	Limits  []Limit  `json:"limits"`
}

// loadPolicy resolves the policy {ref} (id or slug) and builds the index.
func (s *Service) loadPolicy(ctx context.Context, ref string) (*policy.Policy, *index, error) {
	idx, err := s.buildIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range idx.policies {
		if p.Meta.ID == ref || p.Meta.Name == ref {
			return p, idx, nil
		}
	}
	return nil, nil, ErrNotFound
}

// PolicyModels returns one row per concrete (model, host) binding this policy
// grants. Resolution mirrors routing: an explicit ModelIDs grant is host- and
// coverage-agnostic (emits on every binding of the model); wildcard and Models
// DSL grants emit only on hosts the policy actually reaches (host-tier: its own
// host; customer: hosts its host-keys cover). Limits are resolved per triple.
func (s *Service) PolicyModels(ctx context.Context, ref string) (PolicyRef, []PolicyBindingRow, error) {
	p, idx, err := s.loadPolicy(ctx, ref)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	rows := []PolicyBindingRow{}
	for _, gb := range idx.grantedBindings(p, models) {
		rlID := p.SelectRateLimitID(gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name)
		rows = append(rows, PolicyBindingRow{
			Provider:  providerRefOf(gb.prov),
			Host:      hostRefOf(gb.host),
			Model:     modelRefOf(gb.model),
			Binding:   bindingViewOf(gb.binding),
			MatchedBy: gb.matchedBy,
			Limits:    idx.limitsOf(rlID),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model.Name != rows[j].Model.Name {
			return rows[i].Model.Name < rows[j].Model.Name
		}
		return rows[i].Host.Name < rows[j].Host.Name
	})
	return policyRefOf(p, idx), rows, nil
}

// grantedBinding is one (provider, model, host) binding a policy grants, with
// the grant ref(s) that produced it. Shared by the models / hosts / rate-limits
// projections so all three resolve grants identically.
type grantedBinding struct {
	prov      *provider.Provider
	provSlug  string
	model     *model.Model
	host      *host.Host
	binding   *binding.Binding
	matchedBy []string
}

// grantedBindings enumerates every (model, host) binding the policy grants.
func (idx *index) grantedBindings(p *policy.Policy, models []*model.Model) []grantedBinding {
	out := []grantedBinding{}
	for _, m := range models {
		prov := idx.providerByID[m.Meta.Owner.ID]
		provSlug := ""
		if prov != nil {
			provSlug = prov.Meta.Name
		}
		for _, b := range idx.bindingsByModel[m.Meta.ID] {
			h, ok := idx.hostByID[b.Spec.HostID]
			if !ok {
				continue
			}
			matched := idx.bindingGrantRefs(p, m, provSlug, h.Meta.ID, h.Meta.Name)
			if len(matched) == 0 {
				continue
			}
			out = append(out, grantedBinding{prov: prov, provSlug: provSlug, model: m, host: h, binding: b, matchedBy: matched})
		}
	}
	return out
}

// bindingGrantRefs returns the grant ref(s) by which the policy grants this
// model on this specific host, or empty when it doesn't. Explicit ModelIDs are
// coverage-agnostic; wildcard/DSL require the host to be policy-reachable.
func (idx *index) bindingGrantRefs(p *policy.Policy, m *model.Model, provSlug, hostID, hostSlug string) []string {
	refs := []string{}
	for _, id := range p.Spec.ModelIDs {
		if id == m.Meta.ID {
			refs = append(refs, canonicalRef(provSlug, m.Meta.Name))
			break
		}
	}
	if idx.policyCoversHost(p, hostID) {
		wildcard := len(p.Spec.ModelIDs) == 0 && len(p.Spec.Models) == 0
		if wildcard {
			if !(modelDeprecated(m) && !p.Spec.IncludeDeprecated) {
				refs = append(refs, "*")
			}
		} else {
			for _, raw := range p.Spec.Models {
				r, err := modelref.Parse(raw)
				if err != nil {
					continue
				}
				if r.Matches(provSlug, m.Meta.Name, hostSlug) {
					refs = append(refs, raw)
				}
			}
		}
	}
	return refs
}

// policyCoversHost reports whether the policy is allowed to serve on a host:
// a host-tier policy serves only its own host; a customer policy serves the
// hosts its host-keys authenticate to.
func (idx *index) policyCoversHost(p *policy.Policy, hostID string) bool {
	if p.Meta.Owner.Kind == meta.OwnerHost {
		return p.Meta.Owner.ID == hostID
	}
	_, ok := idx.policyKeyHosts(p)[hostID]
	return ok
}

func canonicalRef(provSlug, modelSlug string) string {
	if provSlug == "" {
		return modelSlug
	}
	return provSlug + "/" + modelSlug
}

// PolicyModelExclusions returns the models this policy does NOT grant, each with
// the reason — the diagnostic complement to PolicyModels. Use it to explain an
// empty grant list (missing bindings, deprecation gate, non-matching DSL).
func (s *Service) PolicyModelExclusions(ctx context.Context, ref string) (PolicyRef, []PolicyModelExclusion, error) {
	p, idx, err := s.loadPolicy(ctx, ref)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	out := []PolicyModelExclusion{}
	for _, m := range models {
		provSlug := ""
		if pr, ok := idx.providerByID[m.Meta.Owner.ID]; ok {
			provSlug = pr.Meta.Name
		}
		_, granted, reason := idx.policyGrantsModel(p, m, provSlug, idx.modelHostSet(m.Meta.ID))
		if granted {
			continue
		}
		out = append(out, PolicyModelExclusion{Model: modelRefOf(m), Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.Name < out[j].Model.Name })
	return policyRefOf(p, idx), out, nil
}

// PolicyHosts returns the hosts this policy can reach. Customer policies reach
// the hosts their host-keys authenticate to (keys folded in per host); a
// host-tier policy also reaches its own host.
func (s *Service) PolicyHosts(ctx context.Context, ref string) (PolicyRef, []PolicyHostRow, error) {
	p, idx, err := s.loadPolicy(ctx, ref)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return PolicyRef{}, nil, err
	}

	// Coverage map: which granted models each host uniquely serves, for the
	// required/optional verdict.
	hostModels := map[string]map[string]struct{}{} // hostID → set(modelID)
	modelHostCount := map[string]int{}             // modelID → # of granting hosts
	for _, gb := range idx.grantedBindings(p, models) {
		if hostModels[gb.host.Meta.ID] == nil {
			hostModels[gb.host.Meta.ID] = map[string]struct{}{}
		}
		if _, seen := hostModels[gb.host.Meta.ID][gb.model.Meta.ID]; !seen {
			hostModels[gb.host.Meta.ID][gb.model.Meta.ID] = struct{}{}
			modelHostCount[gb.model.Meta.ID]++
		}
	}

	keysByHost := map[string][]HostKeyRef{}
	hostIDs := map[string]struct{}{}
	for _, id := range p.Spec.HostKeyIDs {
		k, ok := idx.hostkeyByID[id]
		if !ok {
			continue
		}
		hostIDs[k.Spec.HostID] = struct{}{}
		keysByHost[k.Spec.HostID] = append(keysByHost[k.Spec.HostID], HostKeyRef{
			ID:                    k.Meta.ID,
			Name:                  k.Meta.Name,
			Enabled:               hostKeyEnabled(k),
			SharedWithPolicyCount: idx.keyShareCount(k.Meta.ID, p.Meta.ID),
		})
	}
	if p.Meta.Owner.Kind == meta.OwnerHost {
		hostIDs[p.Meta.Owner.ID] = struct{}{}
	}
	rows := []PolicyHostRow{}
	for hid := range hostIDs {
		h, ok := idx.hostByID[hid]
		if !ok {
			continue
		}
		keys := keysByHost[hid]
		if keys == nil {
			keys = []HostKeyRef{}
		}
		rows = append(rows, PolicyHostRow{
			Host:        hostRefOf(h),
			HostKeys:    keys,
			Requirement: requirementOf(hostModels[hid], modelHostCount),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host.Name < rows[j].Host.Name })
	return policyRefOf(p, idx), rows, nil
}

// requirementOf is "required" when the host uniquely serves at least one
// granted model (cover count 1), else "optional".
func requirementOf(served map[string]struct{}, modelHostCount map[string]int) string {
	for modelID := range served {
		if modelHostCount[modelID] <= 1 {
			return "required"
		}
	}
	return "optional"
}

// keyShareCount counts policies OTHER than excludeID whose HostKeyIDs include
// the given key.
func (idx *index) keyShareCount(keyID, excludeID string) int {
	n := 0
	for _, p := range idx.policies {
		if p.Meta.ID == excludeID {
			continue
		}
		for _, id := range p.Spec.HostKeyIDs {
			if id == keyID {
				n++
				break
			}
		}
	}
	return n
}

// UnthrottledModel — a model the policy grants on which no rate-limit applies
// (every granted binding of it resolves to an empty limit set).
type UnthrottledModel struct {
	Model ModelRef `json:"model"`
}

// RateLimitOverlap — a binding claimed by more than one RLBinding. Winner is
// the rate-limit that actually applies (first declared match); losers are the
// ones it shadowed on that binding. All ids match PolicyRateLimitRow.ID.
type RateLimitOverlap struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Host     string   `json:"host"`
	Winner   string   `json:"winner"`
	Losers   []string `json:"losers"`
}

// PolicyRateLimitsView is the rate-limits tab payload: the referenced rule sets
// plus the two derivations the server now owns — models that slip through
// uncapped, and bindings where RLBindings overlap.
type PolicyRateLimitsView struct {
	RateLimits  []PolicyRateLimitRow `json:"rateLimits"`
	Unthrottled []UnthrottledModel   `json:"unthrottled"`
	Overlaps    []RateLimitOverlap   `json:"overlaps"`
}

// PolicyRateLimits returns the rate-limit rule sets the policy references — each
// per-model RLBinding in declared order, then the flat default last — plus the
// unthrottled-models and overlapping-binding derivations.
func (s *Service) PolicyRateLimits(ctx context.Context, ref string) (PolicyRef, PolicyRateLimitsView, error) {
	p, idx, err := s.loadPolicy(ctx, ref)
	if err != nil {
		return PolicyRef{}, PolicyRateLimitsView{}, err
	}
	models, err := s.Models.List(ctx)
	if err != nil {
		return PolicyRef{}, PolicyRateLimitsView{}, err
	}
	view := PolicyRateLimitsView{
		RateLimits:  idx.policyRateLimitRows(p),
		Unthrottled: []UnthrottledModel{},
		Overlaps:    []RateLimitOverlap{},
	}

	capped := map[string]bool{} // modelID → has a non-empty limit on some binding
	uncapped := map[string]*model.Model{}
	for _, gb := range idx.grantedBindings(p, models) {
		// Overlap: every RLBinding (in declared order) that matches this triple.
		var matching []string
		for _, rb := range p.Spec.RLBindings {
			if modelref.MatchAny(rb.Models, gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name) {
				matching = append(matching, rb.RateLimitID)
			}
		}
		if len(matching) > 1 {
			view.Overlaps = append(view.Overlaps, RateLimitOverlap{
				Provider: gb.provSlug, Model: gb.model.Meta.Name, Host: gb.host.Meta.Name,
				Winner: matching[0], Losers: matching[1:],
			})
		}
		// Unthrottled bookkeeping: a model is capped if ANY granted binding
		// resolves to a non-empty limit set.
		rlID := p.SelectRateLimitID(gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name)
		if len(idx.limitsOf(rlID)) > 0 {
			capped[gb.model.Meta.ID] = true
		} else {
			uncapped[gb.model.Meta.ID] = gb.model
		}
	}
	for id, m := range uncapped {
		if !capped[id] {
			view.Unthrottled = append(view.Unthrottled, UnthrottledModel{Model: modelRefOf(m)})
		}
	}
	sort.Slice(view.Unthrottled, func(i, j int) bool { return view.Unthrottled[i].Model.Name < view.Unthrottled[j].Model.Name })
	sort.Slice(view.Overlaps, func(i, j int) bool {
		if view.Overlaps[i].Model != view.Overlaps[j].Model {
			return view.Overlaps[i].Model < view.Overlaps[j].Model
		}
		return view.Overlaps[i].Host < view.Overlaps[j].Host
	})
	return policyRefOf(p, idx), view, nil
}

func (idx *index) rlRow(rlID string, models []string, isDefault bool) PolicyRateLimitRow {
	row := PolicyRateLimitRow{ID: rlID, Default: isDefault, Models: models, Limits: idx.limitsOf(rlID)}
	if rl, ok := idx.rlByID[rlID]; ok {
		row.Name = rl.Meta.Name
	}
	return row
}

func policyRefOf(p *policy.Policy, idx *index) PolicyRef {
	return PolicyRef{
		ID:          p.Meta.ID,
		Name:        p.Meta.Name,
		DisplayName: p.Meta.DisplayName,
		Owner:       idx.ownerRefOf(p.Meta.Owner),
		Enabled:     p.IsEnabled(),
	}
}
