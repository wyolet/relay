package plan

import (
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// Caller rung positions in CallerPlan.Rungs, outermost first.
const (
	rungTeam = iota
	rungProject
	rungPrincipal
)

// CallerPlan is the caller half of the ladder folded for one principal (a user or a service account). Every key of the principal points at it, so a key write or rotation touches no allowance.
type CallerPlan struct {
	Principal string // "user:<id>" or "serviceaccount:<id>"
	Rungs     [3]Rung
	// Refused is set when a rung fails the principal closed: its policy is disabled, its binding ambiguous, or the subject itself disabled.
	Refused *Refusal
	Allow   AllowSet
	// Pools are the pools the principal reaches, in failover order: the innermost rung that links pools or credentials orders them and outer linking rungs narrow them; with no linking rung, the credentials its own scopes own, each a pool of one.
	Pools []*PoolPlan
	// Counters are the caller rungs' rules without a model scope and their budgets, outer rung first.
	Counters []limits.Counter
	Routes   *RouteTable
	scoped   []scopedCounters
	budgetAt int // index into Rungs of the first rung with a budget, -1 for none
	linkedAt int // index into Rungs of the rung deciding reach, -1 for ownership
	in       inputs
}

func (p *CallerPlan) builtFrom() inputs { return p.in }

// Team and Project return the principal's team and project rungs; their SubjectID is empty for a user.
func (p *CallerPlan) Team() *Rung    { return &p.Rungs[rungTeam] }
func (p *CallerPlan) Project() *Rung { return &p.Rungs[rungProject] }

// PrincipalRung returns the principal's own rung.
func (p *CallerPlan) PrincipalRung() *Rung { return &p.Rungs[rungPrincipal] }

// InnermostPolicy returns the policy bound closest to the principal, nil when no caller rung has one. Usage rows record it.
func (p *CallerPlan) InnermostPolicy() *policy.Policy {
	for i := len(p.Rungs) - 1; i >= 0; i-- {
		if ep := p.Rungs[i].Policy; ep != nil {
			return ep.Policy
		}
	}
	return nil
}

// ID returns the subject's id without its kind prefix ("team:<id>" → "<id>").
func (r *Rung) ID() string {
	_, id, _ := strings.Cut(r.SubjectID, ":")
	return id
}

// scopedCounters are a caller rule's counters with the models they apply to.
type scopedCounters struct {
	scope    *LinkedRateLimit
	counters []limits.Counter
}

// AllowSet is the intersection of the caller rungs' model lists; a rung without one does not narrow.
type AllowSet struct {
	grants            [3]*Grant
	includeDeprecated bool
}

// noGrant marks a deprecated model refused because no caller rung names models and none includes deprecated ones.
const noGrant = len(CallerPlan{}.Rungs)

// rejects returns the index of the first rung whose grant does not cover the model at the host, noGrant for a deprecated model nothing granted, or -1 when allowed.
func (a *AllowSet) rejects(providerSlug string, m *model.Model, hostSlug string) int {
	named := false
	for i, g := range a.grants {
		if g == nil {
			continue
		}
		named = true
		if !g.Allows(providerSlug, m, hostSlug) {
			return i
		}
	}
	if !named && !a.includeDeprecated && appcatalog.ModelDeprecated(m) {
		return noGrant
	}
	return -1
}

func (c *controllers) callerPlan(tx *engine.Txn, principal string) error {
	kind, id, _ := strings.Cut(principal, ":")
	cp := &CallerPlan{Principal: principal, budgetAt: -1, linkedAt: -1}
	cp.Rungs = [3]Rung{{Kind: limits.RungTeam}, {Kind: limits.RungProject}, {Kind: limits.RungPrincipal, SubjectID: principal}}
	var disabled [3]bool
	var scopes []string
	switch kind {
	case "serviceaccount":
		sa := engine.Get[serviceaccount.ServiceAccount](tx, id)
		if sa == nil {
			engine.Drop(tx)
			return nil
		}
		proj := engine.Get[project.Project](tx, sa.Spec.ProjectID)
		if proj == nil {
			tx.Issue(parentMissing(engine.Ref{Kind: KindServiceAccount, ID: id}, "spec.projectId", engine.Ref{Kind: KindProject, ID: sa.Spec.ProjectID}))
			engine.Drop(tx)
			return nil
		}
		tm := engine.Get[team.Team](tx, proj.Spec.TeamID)
		if tm == nil {
			tx.Issue(parentMissing(engine.Ref{Kind: KindProject, ID: proj.Meta.ID}, "spec.teamId", engine.Ref{Kind: KindTeam, ID: proj.Spec.TeamID}))
			engine.Drop(tx)
			return nil
		}
		cp.Rungs[rungTeam].SubjectID, cp.Rungs[rungTeam].Name = "team:"+tm.Meta.ID, tm.Meta.Name
		cp.Rungs[rungProject].SubjectID, cp.Rungs[rungProject].Name = "project:"+proj.Meta.ID, proj.Meta.Name
		cp.Rungs[rungPrincipal].Name = sa.Meta.Name
		disabled = [3]bool{!tm.IsEnabled(), !proj.IsEnabled(), !sa.IsEnabled()}
		scopes = []string{cp.Rungs[rungProject].SubjectID, cp.Rungs[rungTeam].SubjectID, "system:"}
		cp.in = inputs{sa, proj, tm}
	case "user":
		u := engine.Get[user.User](tx, id)
		if u == nil {
			engine.Drop(tx)
			return nil
		}
		disabled[rungPrincipal] = u.Disabled
		scopes = []string{principal, "system:"}
		// The user row also carries login state; only the disabled flag shapes the plan.
		cp.in = inputs{u.ID, u.Disabled}
	default:
		engine.Drop(tx)
		return nil
	}
	// Counters of one principal share the team's atomicity group (the user's, without a team), so a team-wide bucket and the principal's own ride one store call.
	group := principal
	if t := cp.Rungs[rungTeam].SubjectID; t != "" {
		group = t
	}
	for i := range cp.Rungs {
		r := &cp.Rungs[i]
		if r.SubjectID == "" {
			continue
		}
		if disabled[i] && cp.Refused == nil {
			cp.Refused = rungRefusal(CodeSubjectDisabled, r, "", r.SubjectID+" is disabled")
		}
		rp := engine.Get[RungPolicy](tx, r.SubjectID)
		cp.in = append(cp.in, rp)
		if rp == nil {
			continue
		}
		r.Policy = rp.Rung.Policy
		if rp.Refused != nil {
			if cp.Refused == nil {
				cp.Refused = rp.Refused
			}
			continue
		}
		cp.foldRung(i, group)
	}
	pools, in := callerPools(tx, cp, scopes)
	cp.Pools = pools
	cp.in = append(cp.in, in...)
	idx := engine.Get[CatalogIndex](tx, "")
	cp.in = append(cp.in, idx)
	if prev := engine.Get[CallerPlan](tx, principal); SameOr(prev, cp) == prev {
		engine.Put(tx, prev)
		return nil
	}
	// One table per principal: the keys of a principal share it; principals with the same rungs fill their own.
	cp.Routes = newRouteTable(cp, idx)
	engine.Put(tx, cp)
	return nil
}

// foldRung adds rung i's grant and counters. Caller counters count per (rule, policy, bound subject), so a policy reused by two subjects gives each its own bucket.
func (cp *CallerPlan) foldRung(i int, group string) {
	r := &cp.Rungs[i]
	ep := r.Policy
	cp.Allow.grants[i] = ep.Grant
	if ep.Policy.Spec.IncludeDeprecated {
		cp.Allow.includeDeprecated = true
	}
	pid := ep.Policy.Meta.ID
	for j := range ep.RateLimits {
		l := &ep.RateLimits[j]
		cs := make([]limits.Counter, 0, len(l.RateLimit.Spec.Rules))
		for _, rule := range l.RateLimit.Spec.Rules {
			cs = append(cs, limits.CallerRule(r.Kind, l.RateLimit, rule, pid, r.SubjectID, group))
		}
		if l.Scoped {
			cp.scoped = append(cp.scoped, scopedCounters{scope: l, counters: cs})
		} else {
			cp.Counters = append(cp.Counters, cs...)
		}
	}
	for _, b := range ep.Budgets {
		cp.Counters = append(cp.Counters, limits.BudgetCounter(r.Kind, b, pid, r.SubjectID, group))
		if cp.budgetAt < 0 {
			cp.budgetAt = i
		}
	}
}

// callerPools resolves the pools the principal reaches. Pools and credentials the policy links but that are missing or cannot serve narrow the list; the policy carries the ref_missing issue.
func callerPools(tx *engine.Txn, cp *CallerPlan, scopes []string) ([]*PoolPlan, inputs) {
	var in inputs
	inner := -1
	for i := len(cp.Rungs) - 1; i >= 0; i-- {
		if cp.Rungs[i].Policy.links() {
			inner = i
			break
		}
	}
	if inner < 0 {
		var pools []*PoolPlan
		for _, sc := range scopes {
			oc := engine.Get[OwnedCredentials](tx, sc)
			in = append(in, oc)
			if oc != nil {
				pools = append(pools, oc.Pools...)
			}
		}
		return pools, in
	}
	cp.linkedAt = inner
	pools := linkedPools(tx, cp.Rungs[inner].Policy, &in)
	for i := inner - 1; i >= 0; i-- {
		if cp.Rungs[i].Policy.links() {
			pools = narrow(pools, linkedPools(tx, cp.Rungs[i].Policy, &in))
		}
	}
	return pools, in
}

func linkedPools(tx *engine.Txn, ep *ExpandedPolicy, in *inputs) []*PoolPlan {
	var out []*PoolPlan
	for _, pid := range ep.Policy.Spec.PoolIDs {
		pp := engine.Get[PoolPlan](tx, pid)
		*in = append(*in, pp)
		if pp != nil {
			out = append(out, pp)
		}
	}
	for _, cid := range ep.Policy.Spec.CredentialIDs {
		s := engine.Get[KeySlot](tx, cid)
		*in = append(*in, s)
		if s != nil {
			out = append(out, s.Alone)
		}
	}
	return out
}

// narrow keeps, in the inner order, only the credentials an outer rung also reaches. A pool losing some keeps its rotation and selection with fewer slots.
func narrow(pools, outer []*PoolPlan) []*PoolPlan {
	allowed := map[string]struct{}{}
	for _, p := range outer {
		for _, s := range p.Slots {
			allowed[s.Credential.Meta.ID] = struct{}{}
		}
	}
	out := pools[:0:0]
	for _, p := range pools {
		kept := make([]*KeySlot, 0, len(p.Slots))
		for _, s := range p.Slots {
			if _, ok := allowed[s.Credential.Meta.ID]; ok {
				kept = append(kept, s)
			}
		}
		switch len(kept) {
		case 0:
		case len(p.Slots):
			out = append(out, p)
		default:
			cp := *p
			cp.Slots = kept
			out = append(out, &cp)
		}
	}
	return out
}
