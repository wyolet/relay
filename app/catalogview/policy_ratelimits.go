package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// PolicyRateLimitRow — one rate-limit rule set the policy links. Default is
// an unscoped link, applying to every model; the others carry the model refs
// they are scoped to.
type PolicyRateLimitRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Default bool     `json:"default"`
	Models  []string `json:"models,omitempty"`
	Limits  []Limit  `json:"limits"`
}

// UnthrottledModel — a model the policy grants on which no rate-limit applies
// (every granted binding of it resolves to an empty limit set).
type UnthrottledModel struct {
	Model ModelRef `json:"model"`
}

// PolicyRateLimitsView is the rate-limits tab payload: the linked rule sets
// plus the models that slip through uncapped. Every matching link applies,
// so links never shadow each other.
type PolicyRateLimitsView struct {
	RateLimits  []PolicyRateLimitRow `json:"rateLimits"`
	Unthrottled []UnthrottledModel   `json:"unthrottled"`
}

// PolicyRateLimits returns the rate-limit rule sets the policy links, in
// declared order, plus the unthrottled-models derivation.
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
	}
	capped := map[string]bool{} // modelID → has a non-empty limit on some binding
	uncapped := map[string]*model.Model{}
	for _, gb := range idx.grantedBindings(p, models) {
		if len(idx.limitsFor(p, gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name)) > 0 {
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
	return policyRefOf(p, idx), view, nil
}

// policyRateLimitRows is shared by PolicyRateLimits and HostPolicies: each
// linked rate limit in declared order.
func (idx *index) policyRateLimitRows(p *policy.Policy) []PolicyRateLimitRow {
	rows := []PolicyRateLimitRow{}
	for _, l := range p.Spec.RateLimits {
		row := PolicyRateLimitRow{ID: l.RateLimitID, Default: len(l.Models) == 0, Models: l.Models, Limits: idx.limitsOf(l.RateLimitID)}
		if rl, ok := idx.rlByID[l.RateLimitID]; ok {
			row.Name = rl.Meta.Name
		}
		rows = append(rows, row)
	}
	return rows
}
