package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
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
//                    that reach it through its pools and credentials
//                    (credential values never exposed).
//   - rate-limits — the rate-limit rule sets the policy links, each with its
//                    model scope (policy_ratelimits.go).

// PolicyRef identifies the policy a sub-resource response belongs to.
type PolicyRef struct {
	ID          string   `json:"id"                    doc:"Policy id."`
	Name        string   `json:"name"                  doc:"Policy slug."`
	DisplayName string   `json:"displayName,omitempty" doc:"Display name."`
	Owner       OwnerRef `json:"owner"                 doc:"The policy's owner."`
	Enabled     bool     `json:"enabled"               doc:"Whether the policy is enabled."`
}

type HostKeyRef struct {
	ID                    string `json:"id"                    doc:"Host key id."`
	Name                  string `json:"name"                  doc:"Host key slug."`
	Enabled               bool   `json:"enabled"               doc:"Whether the key is enabled."`
	SharedWithPolicyCount int    `json:"sharedWithPolicyCount" doc:"How many other policies link this key, directly or through a pool."` // # of OTHER policies linking this key, directly or through a pool
	ReadOnly              bool   `json:"readOnly,omitempty"    doc:"The key belongs to the real account of a shared account one of the policy's pools draws on: it counts as reaching the host, but is never edited or revealed through the share."`
}

// PolicyBindingRow — one concrete (provider, model, host) binding this policy
// grants, fully pre-joined. Mirrors ModelHostRow: the frontend renders rows
// and groups by host client-side, no catalog round-trip. MatchedBy carries the
// grant ref(s) that produced this row (raw Spec.Models entries, or "*" for an
// empty model list) — the one fact the client can't recompute. Limits are
// resolved for THIS exact triple.
type PolicyBindingRow struct {
	Provider  ProviderRef `json:"provider"  doc:"The model's provider."`
	Host      HostRef     `json:"host"      doc:"The serving host."`
	Model     ModelRef    `json:"model"     doc:"The model."`
	Binding   BindingView `json:"binding"   doc:"The binding of the model at the host."`
	MatchedBy []string    `json:"matchedBy" doc:"The policy's model entries that allow this binding; * when the policy lists no models."`
	Limits    []Limit     `json:"limits"    doc:"Rules of every rate limit the policy applies to this binding."`
}

// PolicyModelExclusion — a model this policy does NOT grant, with why. Surfaced
// by PolicyModelExclusions (the ?debug view) to make an empty grant diagnosable
// — e.g. a host-tier policy whose host has no bindings post-migration.
type PolicyModelExclusion struct {
	Model  ModelRef `json:"model"  doc:"The model."`
	Reason string   `json:"reason" doc:"Why the policy does not allow it, such as no matching entry or no host the policy reaches."`
}

// PolicyHostRow — a host this policy can reach, with the host-keys that reach
// it (empty for a host-tier policy's own host, which needs no key to qualify).
// Requirement is "required" when this host uniquely serves at least one model
// the policy grants (losing its key would drop that model), else "optional" —
// a sibling reachable host covers everything it does.
type PolicyHostRow struct {
	Host        HostRef      `json:"host"        doc:"The host."`
	HostKeys    []HostKeyRef `json:"hostKeys"    doc:"Host keys reaching it through the policy's pools and keys; empty for a tier preset's own host."`
	Requirement string       `json:"requirement" enum:"required,optional" doc:"required when the host alone serves some allowed model, so losing its keys would drop that model; optional when other hosts cover everything it serves."` // "required" | "optional"
}

// loadPolicy resolves the policy {ref} (id or slug) and builds the index.
// A policy the caller may not see resolves as ErrNotFound (existence must
// not be confirmed), which gates every policy-rooted projection.
func (s *Service) loadPolicy(ctx context.Context, ref string) (*policy.Policy, *index, error) {
	idx, err := s.buildIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range idx.policies {
		if p.Meta.ID == ref || p.Meta.Name == ref {
			if !s.visible("policy", p.Meta.ID, p.Meta.Owner) {
				return nil, nil, ErrNotFound
			}
			return p, idx, nil
		}
	}
	return nil, nil, ErrNotFound
}

// PolicyModels returns one row per concrete (model, host) binding this policy
// grants, on the hosts it reaches. Limits are resolved per triple.
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
		rows = append(rows, PolicyBindingRow{
			Provider:  providerRefOf(gb.prov),
			Host:      hostRefOf(gb.host),
			Model:     modelRefOf(gb.model),
			Binding:   bindingViewOf(gb.binding),
			MatchedBy: gb.matchedBy,
			Limits:    idx.limitsFor(p, gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name),
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
// model on this specific host, or empty when it doesn't.
func (idx *index) bindingGrantRefs(p *policy.Policy, m *model.Model, provSlug, hostID, hostSlug string) []string {
	refs := []string{}
	if !idx.policyCoversHost(p, hostID) {
		return refs
	}
	if len(p.Spec.Models) == 0 {
		return append(refs, "*")
	}
	for _, raw := range p.Spec.Models {
		r, err := modelref.Parse(raw)
		if err != nil {
			continue
		}
		if r.Matches(provSlug, m.Meta.Name, hostSlug) {
			refs = append(refs, raw)
		}
	}
	return refs
}

// PolicyModelExclusions returns the models this policy does NOT grant, each with
// the reason — the diagnostic complement to PolicyModels. Use it to explain an
// empty grant list (missing bindings, non-matching DSL).
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

// PolicyHosts returns the hosts this policy can reach: the hosts of its
// pools and credentials (keys folded in per host), and a host-tier policy's
// own host. A policy linking neither lists only the hosts it grants models on.
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
	for _, link := range idx.policyCredentials(p) {
		k, ok := idx.hostkeyByID[link.id]
		if !ok {
			continue
		}
		hostIDs[k.Spec.HostID] = struct{}{}
		keysByHost[k.Spec.HostID] = append(keysByHost[k.Spec.HostID], HostKeyRef{
			ID:                    k.Meta.ID,
			Name:                  k.Meta.Name,
			Enabled:               hostKeyEnabled(k),
			SharedWithPolicyCount: idx.keyShareCount(k.Meta.ID, p.Meta.ID),
			ReadOnly:              link.readOnly,
		})
	}
	for hid := range idx.policyReachHosts(p) {
		hostIDs[hid] = struct{}{}
	}
	if len(p.Spec.PoolIDs) == 0 {
		for hid := range hostModels {
			hostIDs[hid] = struct{}{}
		}
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

// keyShareCount counts policies OTHER than excludeID that link the given
// key, directly or through a pool.
func (idx *index) keyShareCount(keyID, excludeID string) int {
	n := 0
	for _, p := range idx.policies {
		if p.Meta.ID == excludeID {
			continue
		}
		for _, link := range idx.policyCredentials(p) {
			if link.id == keyID {
				n++
				break
			}
		}
	}
	return n
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
