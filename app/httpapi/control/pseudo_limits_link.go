// The link options of POST /pseudo-limits: the model scope checked as a policy update checks a rate-limit link's models, and the link answered as the policy stores it.

package control

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// PseudoLimitRef is the budget or rate limit made, and how the policy links it.
type PseudoLimitRef struct {
	ID   string          `json:"id"   doc:"Id of the budget or rate limit."`
	Name string          `json:"name" doc:"Slug of the budget or rate limit."`
	Link PseudoLimitLink `json:"link" doc:"The link as the policy stores it."`
}

// PseudoLimitLink is a budgets or rateLimits entry of the policy, as stored.
type PseudoLimitLink struct {
	Shared bool     `json:"shared"           doc:"True counts the limit once for everyone under the target; false once per account under it."`
	Models []string `json:"models,omitempty" doc:"The models a rate limit applies to, canonical; omitted for every model, and for a budget."`
}

// checkLinkModels checks a rate limit's spec.link.models with the rules a policy update applies to a rate-limit link's models (the policy's own Validate, then the catalog check), and replaces them with their canonical form; a refusal points at /spec/link/models. owner and name stand in for the policy's, which the policy step decides later.
func (d Deps) checkLinkModels(body *pseudo.Limits, owner meta.Owner, name string) error {
	l := body.Spec.Link
	if body.Spec.RateLimit == nil || l == nil || len(l.Models) == 0 {
		return nil
	}
	probe := policy.Policy{Meta: meta.Metadata{Name: name, Owner: owner}, Spec: policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: ids.New(), Models: l.Models}}}}
	if err := probe.Validate(); err != nil {
		return refusal(huma.Error400BadRequest(fmt.Sprintf("spec.link.models: %v", err)), "/spec/link/models")
	}
	idx, err := loadResolveIndex(d)
	if err != nil {
		return huma.Error500InternalServerError("load catalog: " + err.Error())
	}
	canonical, err := normalizePolicyRefs(idx, l.Models, name, "spec.link.models")
	if err != nil {
		return refusal(err, "/spec/link/models")
	}
	l.Models = canonical
	return nil
}

// limitRef answers the limit c made with its link as the policy c leaves the account under holds it.
func limitRef(c pseudo.LimitsChange) *PseudoLimitRef {
	ref := &PseudoLimitRef{ID: c.Limit.Meta.ID, Name: c.Limit.Meta.Name, Link: PseudoLimitLink{Shared: true}}
	if c.Policy == nil {
		return ref
	}
	for _, l := range c.Policy.Spec.RateLimits {
		if l.RateLimitID == ref.ID {
			ref.Link = PseudoLimitLink{Shared: l.IsShared(), Models: l.Models}
		}
	}
	for _, l := range c.Policy.Spec.Budgets {
		if l.BudgetID == ref.ID {
			ref.Link = PseudoLimitLink{Shared: l.IsShared()}
		}
	}
	return ref
}
