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

// CallerPlan is the caller half of the ladder folded for one principal (a user or a service account), or for a user acting in a project through a token (ProjectPlan.CallerFor). Every key of the principal points at it, so a key write or rotation touches no allowance.
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
	// hosts governs requests that bring their own upstream key (ResolveUnkeyed).
	hosts    *HostRungs
	scoped   []scopedCounters
	budgetAt int // index into Rungs of the first rung with a budget, -1 for none
	linkedAt int // index into Rungs of the rung deciding reach, -1 for ownership
	reads    callerReads
}

func (p *CallerPlan) builtFrom() inputs { return p.reads.in }

// Team and Project return the principal's team and project rungs; their SubjectID is empty for a user outside a project.
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

// rungRead is one caller rung as the fold needs it, read once through the engine.
type rungRead struct {
	rung     Rung
	disabled bool
	// refused is set when the rung's binding refuses: its policy disabled, or its subject bound twice.
	refused *Refusal
	// linked is what the rung's policy links, read when it links pools or credentials.
	linked []*PoolPlan
}

// callerReads is everything a caller fold reads. The plan keeps it so a token's plan can fold the user's own rung under a project's and its team's (ProjectPlan.CallerFor) without the engine.
type callerReads struct {
	rungs [3]rungRead
	// owned holds what the principal's, the project's, the team's and the system's scopes own, the reach when no rung links; read only then.
	owned [4]*OwnedCredentials
	idx   *CatalogIndex
	hosts *HostRungs
	in    inputs
}

// Owner scopes in callerReads.owned, innermost first.
const (
	ownedPrincipal = iota
	ownedProject
	ownedTeam
	ownedSystem
)

func (rd *callerReads) links() bool {
	for i := range rd.rungs {
		if rd.rungs[i].rung.Policy.links() {
			return true
		}
	}
	return false
}

func (c *controllers) callerPlan(tx *engine.Txn, principal string) error {
	kind, id, _ := strings.Cut(principal, ":")
	var rd callerReads
	rd.rungs = [3]rungRead{{rung: Rung{Kind: limits.RungTeam}}, {rung: Rung{Kind: limits.RungProject}}, {rung: Rung{Kind: limits.RungPrincipal, SubjectID: principal}}}
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
		rd.rungs[rungTeam], rd.rungs[rungProject] = teamRung(tm), projectRung(proj)
		rd.rungs[rungPrincipal].rung.Name, rd.rungs[rungPrincipal].disabled = sa.Meta.Name, !sa.IsEnabled()
		rd.in = inputs{sa, proj, tm}
	case "user":
		u := engine.Get[user.User](tx, id)
		if u == nil {
			engine.Drop(tx)
			return nil
		}
		rd.rungs[rungPrincipal].disabled = u.Disabled
		// The user row also carries login state; only the disabled flag shapes the plan.
		rd.in = inputs{u.ID, u.Disabled}
	default:
		engine.Drop(tx)
		return nil
	}
	for i := range rd.rungs {
		readRung(tx, &rd.rungs[i], &rd.in)
	}
	if !rd.links() {
		// A service account owns nothing; a user's own scope is its principal.
		if kind == "user" {
			rd.owned[ownedPrincipal] = readOwned(tx, principal, &rd.in)
		}
		if p := rd.rungs[rungProject].rung.SubjectID; p != "" {
			rd.owned[ownedProject] = readOwned(tx, p, &rd.in)
			rd.owned[ownedTeam] = readOwned(tx, rd.rungs[rungTeam].rung.SubjectID, &rd.in)
		}
		rd.owned[ownedSystem] = readOwned(tx, "system:", &rd.in)
	}
	rd.idx = engine.Get[CatalogIndex](tx, "")
	rd.hosts = engine.Get[HostRungs](tx, "")
	rd.in = append(rd.in, rd.idx, rd.hosts)
	if prev := engine.Get[CallerPlan](tx, principal); prev != nil && prev.reads.in.equal(rd.in) {
		engine.Put(tx, prev)
		return nil
	}
	engine.Put(tx, foldCaller(principal, rd))
	return nil
}

func teamRung(tm *team.Team) rungRead {
	return rungRead{rung: Rung{Kind: limits.RungTeam, SubjectID: "team:" + tm.Meta.ID, Name: tm.Meta.Name}, disabled: !tm.IsEnabled()}
}

func projectRung(p *project.Project) rungRead {
	return rungRead{rung: Rung{Kind: limits.RungProject, SubjectID: "project:" + p.Meta.ID, Name: p.Meta.Name}, disabled: !p.IsEnabled()}
}

// readRung reads the policy bound to the rung's subject and, when the policy links pools or credentials, what it links.
func readRung(tx *engine.Txn, r *rungRead, in *inputs) {
	if r.rung.SubjectID == "" {
		return
	}
	rp := engine.Get[RungPolicy](tx, r.rung.SubjectID)
	*in = append(*in, rp)
	if rp == nil {
		return
	}
	r.rung.Policy, r.refused = rp.Rung.Policy, rp.Refused
	if r.rung.Policy.links() {
		r.linked = linkedPools(tx, r.rung.Policy, in)
	}
}

func readOwned(tx *engine.Txn, scope string, in *inputs) *OwnedCredentials {
	oc := engine.Get[OwnedCredentials](tx, scope)
	*in = append(*in, oc)
	return oc
}

// foldCaller folds read rungs into a caller plan; it reads nothing more, so a token's plan folds the same way off the engine.
func foldCaller(principal string, rd callerReads) *CallerPlan {
	cp := &CallerPlan{Principal: principal, budgetAt: -1, linkedAt: -1, hosts: rd.hosts, reads: rd}
	for i := range rd.rungs {
		cp.Rungs[i] = rd.rungs[i].rung
	}
	for i := range cp.Rungs {
		r := &cp.Rungs[i]
		if r.SubjectID == "" {
			continue
		}
		if rd.rungs[i].disabled && cp.Refused == nil {
			cp.Refused = rungRefusal(CodeSubjectDisabled, r, "", r.SubjectID+" is disabled")
		}
		if refused := rd.rungs[i].refused; refused != nil {
			if cp.Refused == nil {
				cp.Refused = refused
			}
			continue
		}
		if r.Policy != nil {
			cp.foldRung(i, counterGroup(principal, cp.Rungs[rungTeam].SubjectID, i))
		}
	}
	cp.Pools = callerPools(cp, &rd)
	// One table per plan: the keys of a principal share it; principals with the same rungs fill their own.
	cp.Routes = newRouteTable(cp, rd.idx)
	return cp
}

// counterGroup is the atomicity group of rung i's counters: the team's when there is one, so a team-wide bucket and the principal's own ride one store call. A user's own rung keeps the user's group in any project a token puts it in, so the user's buckets stay one wherever the user calls from.
func counterGroup(principal, team string, i int) string {
	if team == "" || (i == rungPrincipal && strings.HasPrefix(principal, "user:")) {
		return principal
	}
	return team
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
func callerPools(cp *CallerPlan, rd *callerReads) []*PoolPlan {
	inner := -1
	for i := len(rd.rungs) - 1; i >= 0; i-- {
		if rd.rungs[i].rung.Policy.links() {
			inner = i
			break
		}
	}
	if inner < 0 {
		var pools []*PoolPlan
		for _, oc := range rd.owned {
			if oc != nil {
				pools = append(pools, oc.Pools...)
			}
		}
		return pools
	}
	cp.linkedAt = inner
	pools := rd.rungs[inner].linked
	for i := inner - 1; i >= 0; i-- {
		if rd.rungs[i].rung.Policy.links() {
			pools = narrow(pools, rd.rungs[i].linked)
		}
	}
	return pools
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
