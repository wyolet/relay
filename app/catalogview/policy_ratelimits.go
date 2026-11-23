package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// PolicyRateLimitRow — one rate-limit rule set the policy links. Default is
// an unscoped link, applying to every model; the others carry the model refs
// they are scoped to.
type PolicyRateLimitRow struct {
	ID      string   `json:"id"               doc:"Rate limit id."`
	Name    string   `json:"name"             doc:"Rate limit slug; empty when hidden."`
	Hidden  bool     `json:"hidden,omitempty" doc:"The caller may not see the rate limit: its name and rules are left out."`
	Default bool     `json:"default"          doc:"The link has no model scope and applies to every model."`
	Models  []string `json:"models,omitempty" doc:"Model references the link is scoped to."`
	Limits  []Limit  `json:"limits"           doc:"The rate limit's rules."`
}

// UnthrottledModel — a model the policy grants on which no rate-limit applies
// (every granted binding of it resolves to an empty limit set).
type UnthrottledModel struct {
	Model ModelRef `json:"model" doc:"An allowed model no linked rate limit covers."`
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
		RateLimits:  idx.policyRateLimitRows(p, s.rateLimitVisible(idx)),
		Unthrottled: []UnthrottledModel{},
	}
	capped := map[string]bool{} // modelID → has a non-empty limit on some binding
	uncapped := map[string]*model.Model{}
	all := func(string) bool { return true }
	for _, gb := range idx.grantedBindings(p, models) {
		if limits, _ := idx.limitsFor(p, gb.provSlug, gb.model.Meta.Name, gb.host.Meta.Name, all); len(limits) > 0 {
			capped[gb.model.Meta.ID] = true
		} else {
			uncapped[gb.model.Meta.ID] = gb.model
		}
	}
	for id, m := range uncapped {
		if !capped[id] {
			view.Unthrottled = append(view.Unthrottled, UnthrottledModel{Model: idx.modelRef(m)})
		}
	}
	sort.Slice(view.Unthrottled, func(i, j int) bool { return view.Unthrottled[i].Model.Name < view.Unthrottled[j].Model.Name })
	return policyRefOf(p, idx), view, nil
}

// policyRateLimitRows is shared by PolicyRateLimits and HostPolicies: each
// linked rate limit in declared order, one visible refuses by its id alone.
func (idx *index) policyRateLimitRows(p *policy.Policy, visible func(string) bool) []PolicyRateLimitRow {
	rows := []PolicyRateLimitRow{}
	for _, l := range p.Spec.RateLimits {
		row := PolicyRateLimitRow{ID: l.RateLimitID, Default: len(l.Models) == 0, Models: l.Models}
		if !visible(l.RateLimitID) {
			row.Hidden, row.Limits = true, []Limit{}
			rows = append(rows, row)
			continue
		}
		row.Limits = idx.limitsOf(l.RateLimitID)
		if rl, ok := idx.rlByID[l.RateLimitID]; ok {
			row.Name = rl.Meta.Name
		}
		rows = append(rows, row)
	}
	return rows
}
