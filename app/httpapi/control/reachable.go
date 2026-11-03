package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
)

// Reachable is what a lane may use, as the request path would route it: the models it reaches and the models at its hosts it may not use.
type Reachable struct {
	Account       *ReachRef       `json:"account,omitempty"       doc:"The account the reach was read for; omitted for a system-owned policy, which no account holds."`
	BoundAccounts []ReachRef      `json:"boundAccounts,omitempty" doc:"For a saved policy: the accounts bound to it that the caller may read."`
	Refused       *ReachRefusal   `json:"refused,omitempty"       doc:"Set when the whole ladder refuses (a disabled policy or scope, personal accounts turned off); models and excluded are then empty."`
	Models        []ReachedModel  `json:"models"                  doc:"Models the lane reaches, by model slug."`
	Excluded      []ExcludedModel `json:"excluded"                doc:"Models bound at a host the lane reaches that it may not use, each with the rung that refuses it."`
}

// ReachRef names a row the reach goes through.
type ReachRef struct {
	ID          string `json:"id"                    doc:"Id (UUIDv7)."`
	Name        string `json:"name"                  doc:"Slug."`
	DisplayName string `json:"displayName,omitempty" doc:"Display name."`
}

// ReachModelRef names a model.
type ReachModelRef struct {
	ReachRef
	Labels map[string]string `json:"labels,omitempty" doc:"The model's labels."`
}

// ReachVia is one pool and account a model is reached through.
type ReachVia struct {
	Pool    *ReachRef `json:"pool,omitempty"    doc:"The pool; omitted for a key reached by its scope's ownership, or a pool the caller may not read."`
	Account *ReachRef `json:"account,omitempty" doc:"The account the keys draw on: a shared account when reached through one; omitted when the keys name none or the caller may not read it."`
}

// ReachUpstream is one provider account whose keys could serve the model and what its tier says.
type ReachUpstream struct {
	Account  ReachRef  `json:"account"            doc:"The provider account holding the keys."`
	SharedAs *ReachRef `json:"sharedAs,omitempty" doc:"The shared account the keys are reached through."`
	Policy   *ReachRef `json:"policy,omitempty"   doc:"The tier policy bound to the account; omitted when none is."`
	Allowed  bool      `json:"allowed"            doc:"Whether the account's tier lets the model through."`
	Reason   string    `json:"reason,omitempty"   doc:"Why not, when allowed is false."`
	Rung     string    `json:"rung,omitempty"     doc:"The rung that refuses, when allowed is false: account."`
}

// ReachRefusal is why a model, or the whole ladder, is refused.
type ReachRefusal struct {
	Code   string `json:"code"           doc:"The refusal code a request would get, such as model_not_allowed or no_credentials."`
	Reason string `json:"reason"         doc:"The refusal in words, naming the rung and policy that decide it."`
	Rung   string `json:"rung,omitempty" doc:"The rung that refuses: team, project, lane (caller rungs) or account (an upstream tier); empty when no rung decides, as for a disabled model."`
}

// ReachedModel is one model the lane reaches.
type ReachedModel struct {
	Provider ReachRef        `json:"provider" doc:"The model's provider."`
	Model    ReachModelRef   `json:"model"    doc:"The model."`
	Hosts    []ReachRef      `json:"hosts"    doc:"Hosts serving it for the lane, in failover order."`
	Via      []ReachVia      `json:"via"      doc:"What brings it, in failover order."`
	Upstream []ReachUpstream `json:"upstream" doc:"Each provider account whose keys could serve it, with its tier's verdict."`
}

// ExcludedModel is one model at a reached host the lane may not use.
type ExcludedModel struct {
	ReachRefusal
	Provider ReachRef        `json:"provider" doc:"The model's provider."`
	Model    ReachModelRef   `json:"model"    doc:"The model."`
	Hosts    []ReachRef      `json:"hosts"    doc:"The reached hosts that bind it."`
	Upstream []ReachUpstream `json:"upstream" doc:"Each provider account whose keys could serve it, with its tier's verdict."`
}

// reachView projects a plan reach for a caller, leaving out rows they may not read.
type reachView struct {
	ctx context.Context
	az  authz.Authorizer
	v   *engine.View
}

func (rv reachView) sees(kind, id string, owner meta.Owner) bool {
	return visibleTo(rv.ctx, rv.az, kind, id, owner)
}

func (rv reachView) project(r *plan.Reach) Reachable {
	out := Reachable{Models: []ReachedModel{}, Excluded: []ExcludedModel{}}
	if a := r.Account; a != nil {
		out.Account = rv.accountRef(a)
	}
	if r.Refused != nil {
		ref := rv.refusal(r.Refused)
		out.Refused = &ref
		return out
	}
	for _, m := range r.Models {
		if !rv.sees("model", m.Model.Meta.ID, m.Model.Meta.Owner) {
			continue
		}
		rm := ReachedModel{Provider: providerRef(m), Model: modelRef(m), Hosts: rv.hosts(m), Via: []ReachVia{}, Upstream: rv.upstream(m.Upstream)}
		for _, via := range m.Via {
			rvia := ReachVia{Account: rv.accountRef(via.Account)}
			if p := via.Pool; p != nil && rv.sees("pool", p.Meta.ID, p.Meta.Owner) {
				rvia.Pool = &ReachRef{ID: p.Meta.ID, Name: p.Meta.Name, DisplayName: p.Meta.DisplayName}
			}
			if rvia.Pool != nil || rvia.Account != nil {
				rm.Via = append(rm.Via, rvia)
			}
		}
		out.Models = append(out.Models, rm)
	}
	for _, m := range r.Excluded {
		if !rv.sees("model", m.Model.Meta.ID, m.Model.Meta.Owner) {
			continue
		}
		out.Excluded = append(out.Excluded, ExcludedModel{ReachRefusal: rv.refusal(m.Refused), Provider: providerRef(m), Model: modelRef(m), Hosts: rv.hosts(m), Upstream: rv.upstream(m.Upstream)})
	}
	return out
}

func (rv reachView) accountRef(a *account.Account) *ReachRef {
	if a == nil || !rv.sees("account", a.Meta.ID, a.Meta.Owner) {
		return nil
	}
	return &ReachRef{ID: a.Meta.ID, Name: a.Meta.Name, DisplayName: a.Meta.DisplayName}
}

func (rv reachView) hosts(m plan.ModelReach) []ReachRef {
	out := []ReachRef{}
	for _, h := range m.Hosts {
		if rv.sees("host", h.Meta.ID, h.Meta.Owner) {
			out = append(out, ReachRef{ID: h.Meta.ID, Name: h.Meta.Name, DisplayName: h.Meta.DisplayName})
		}
	}
	return out
}

// upstream lists the accounts the caller may read, with their tier policies when readable too.
func (rv reachView) upstream(cs []plan.UpstreamCheck) []ReachUpstream {
	out := []ReachUpstream{}
	for _, c := range cs {
		acc := rv.accountRef(c.Account)
		if acc == nil {
			continue
		}
		u := ReachUpstream{Account: *acc, SharedAs: rv.accountRef(c.SharedAs), Allowed: c.Refused == nil}
		if p := c.Policy; p != nil && rv.sees("policy", p.Meta.ID, p.Meta.Owner) {
			u.Policy = &ReachRef{ID: p.Meta.ID, Name: p.Meta.Name, DisplayName: p.Meta.DisplayName}
		}
		if c.Refused != nil {
			ref := rv.refusal(c.Refused)
			u.Reason, u.Rung = ref.Reason, ref.Rung
		}
		out = append(out, u)
	}
	return out
}

// refusal words r, naming the deciding policy only when the caller may read it.
func (rv reachView) refusal(r *plan.Refusal) ReachRefusal {
	out := ReachRefusal{Code: r.Code, Reason: r.Message, Rung: string(r.Rung)}
	if r.PolicyID == "" {
		return out
	}
	if owner := plan.OwnerOf(rv.v, engine.Ref{Kind: plan.KindPolicy, ID: r.PolicyID}); rv.sees("policy", r.PolicyID, owner) {
		out.Reason = plan.ReasonOf(rv.v, r)
	}
	return out
}

func providerRef(m plan.ModelReach) ReachRef {
	if p := m.Provider; p != nil {
		return ReachRef{ID: p.Meta.ID, Name: p.Meta.Name, DisplayName: p.Meta.DisplayName}
	}
	return ReachRef{}
}

func modelRef(m plan.ModelReach) ReachModelRef {
	md := m.Model.Meta
	return ReachModelRef{ReachRef: ReachRef{ID: md.ID, Name: md.Name, DisplayName: md.DisplayName}, Labels: md.Labels}
}
