package catalogview

import (
	"sort"

	"context"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
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
	ID   string `json:"id"`
	Name string `json:"name"`
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
type PolicyHostRow struct {
	Host     HostRef      `json:"host"`
	HostKeys []HostKeyRef `json:"hostKeys"`
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
			rlID := p.SelectRateLimitID(provSlug, m.Meta.Name, h.Meta.Name)
			rows = append(rows, PolicyBindingRow{
				Provider:  providerRefOf(prov),
				Host:      hostRefOf(h),
				Model:     modelRefOf(m),
				Binding:   bindingViewOf(b),
				MatchedBy: matched,
				Limits:    idx.limitsOf(rlID),
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Model.Name != rows[j].Model.Name {
			return rows[i].Model.Name < rows[j].Model.Name
		}
		return rows[i].Host.Name < rows[j].Host.Name
	})
	return policyRefOf(p, idx), rows, nil
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
	keysByHost := map[string][]HostKeyRef{}
	hostIDs := map[string]struct{}{}
	for _, id := range p.Spec.HostKeyIDs {
		k, ok := idx.hostkeyByID[id]
		if !ok {
			continue
		}
		hostIDs[k.Spec.HostID] = struct{}{}
		keysByHost[k.Spec.HostID] = append(keysByHost[k.Spec.HostID], HostKeyRef{ID: k.Meta.ID, Name: k.Meta.Name})
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
		rows = append(rows, PolicyHostRow{Host: hostRefOf(h), HostKeys: keys})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Host.Name < rows[j].Host.Name })
	return policyRefOf(p, idx), rows, nil
}

// PolicyRateLimits returns the rate-limit rule sets the policy references — each
// per-model RLBinding in declared order, then the flat default last. Declared
// order is significant (first-match wins), so the list is not re-sorted.
func (s *Service) PolicyRateLimits(ctx context.Context, ref string) (PolicyRef, []PolicyRateLimitRow, error) {
	p, idx, err := s.loadPolicy(ctx, ref)
	if err != nil {
		return PolicyRef{}, nil, err
	}
	return policyRefOf(p, idx), idx.policyRateLimitRows(p), nil
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
