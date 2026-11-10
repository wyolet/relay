package plan

import (
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
)

// Caller rung positions in CallerPlan.Rungs, outermost first.
const (
	rungTeam = iota
	rungProject
	rungLane
)

// CallerPlan is the caller half of the ladder folded for one lane: the account a request runs in and is charged to. A member or service account in project P of team T climbs T's account, then P's, then the lane; a member of team T climbs T's account, then the lane; a personal account is the lane alone. A provider account used as a lane climbs nothing: its own policy applies as the upstream rung of its keys, which are all it reaches. Every key whose default lane this is points at it, so a key write or rotation touches no allowance.
type CallerPlan struct {
	Lane *account.Account
	// Holder is the principal acting in the lane ("user:<id>" or "serviceaccount:<id>"); empty for a provider account.
	Holder string
	Rungs  [3]Rung
	// Refused is set when a rung fails the lane closed: its policy is disabled, its binding ambiguous, its team, project or holder disabled, or the lane closed.
	Refused *Refusal
	Allow   AllowSet
	// Pools are the pools the lane reaches, in failover order: the innermost rung that links pools orders them and outer linking rungs narrow them; with no linking rung, the credentials the lane's scopes own, each a pool of one.
	Pools []*PoolPlan
	// Counters are the caller rungs' rules without a model scope and their budgets, outer rung first.
	Counters []limits.Counter
	Routes   *RouteTable
	// PayloadLogging is the nearest account on the ladder that sets it, lane first; empty leaves capture to the key.
	PayloadLogging account.PayloadLogging
	scoped         []scopedCounters
	budgetAt       int // index into Rungs of the first rung with a budget, -1 for none
	linkedAt       int // index into Rungs of the rung deciding reach, -1 for ownership
	in             inputs
}

func (p *CallerPlan) builtFrom() inputs { return p.in }

// Team and Project return the lane's team and project rungs; their SubjectID is empty when the lane sits in no team or project.
func (p *CallerPlan) Team() *Rung    { return &p.Rungs[rungTeam] }
func (p *CallerPlan) Project() *Rung { return &p.Rungs[rungProject] }

// LaneRung returns the lane's own rung.
func (p *CallerPlan) LaneRung() *Rung { return &p.Rungs[rungLane] }

// InnermostPolicy returns the policy bound closest to the lane, nil when no caller rung has one. Usage rows record it.
func (p *CallerPlan) InnermostPolicy() *policy.Policy {
	for i := len(p.Rungs) - 1; i >= 0; i-- {
		if ep := p.Rungs[i].Policy; ep != nil {
			return ep.Policy
		}
	}
	return nil
}

// CapturesPayload reports whether a request in this lane has its bodies captured: the nearest account on the ladder that sets payloadLogging decides, else the key's own opt-in.
func (p *CallerPlan) CapturesPayload(keyOptIn bool) bool {
	switch p.PayloadLogging {
	case account.PayloadLoggingOn:
		return true
	case account.PayloadLoggingOff:
		return false
	}
	return keyOptIn
}

// ID returns the id of the team or project the rung stands for, "" for a rung the lane does not climb.
func (r *Rung) ID() string { return r.ScopeID }

// scopedCounters are a caller rule's counters with the models they apply to.
type scopedCounters struct {
	scope    *LinkedRateLimit
	counters []limits.Counter
}

// AllowSet is the intersection of the caller rungs' model lists; a rung without one does not narrow.
type AllowSet struct {
	grants [3]*Grant
}

// rejects returns the index of the first rung whose grant does not cover the model at the host, or -1 when allowed.
func (a *AllowSet) rejects(providerSlug string, m *model.Model, hostSlug string) int {
	for i, g := range a.grants {
		if g != nil && !g.Allows(providerSlug, m, hostSlug) {
			return i
		}
	}
	return -1
}

// rungRead is one caller rung as the fold needs it, read once through the engine.
type rungRead struct {
	rung     Rung
	disabled bool
	closed   bool
	// refused is set when the rung's binding refuses: its policy disabled, or its account bound twice.
	refused *Refusal
	// linked is what the rung's policy links, read when it links pools.
	linked []*PoolPlan
}

// callerReads is everything a caller fold reads.
type callerReads struct {
	rungs [3]rungRead
	// owned holds what the lane's scopes own and what is shared with them, innermost first: the reach when no rung links; read only then.
	owned [][]*PoolPlan
	idx   *CatalogIndex
	in    inputs
}

func (rd *callerReads) links() bool {
	for i := range rd.rungs {
		if rd.rungs[i].rung.Policy.links() {
			return true
		}
	}
	return false
}

func (c *controllers) callerPlan(tx *engine.Txn, subject string) error {
	_, id, _ := strings.Cut(subject, ":")
	acc := engine.Get[account.Account](tx, id)
	if acc == nil {
		engine.Drop(tx)
		return nil
	}
	kind := acc.DerivedKind()
	if kind == account.KindPersonal || kind == account.KindProvider {
		if off := personalAccountOff(tx, acc.Meta.Owner); off != nil {
			// A provider account carries this issue from its account plan already.
			if kind == account.KindPersonal {
				tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssuePersonalAccountOff, Source: engine.Ref{Kind: KindAccount, ID: acc.Meta.ID}, Field: "metadata.owner",
					Message: "personal accounts are turned off for " + off.name() + "; this account and its keys serve nothing until they are on again"})
			}
			cp := refusedPlan(acc, personalOffRefusal(off, subject))
			if h := acc.Spec.Holder; h != nil {
				cp.Holder = h.Key()
			}
			cp.in = inputs{acc, off}
			engine.Put(tx, SameOr(engine.Get[CallerPlan](tx, subject), cp))
			return nil
		}
	}
	// A team's or project's own account is a rung above lanes, never a lane itself.
	if kind == account.KindTeam || kind == account.KindProject {
		engine.Drop(tx)
		return nil
	}
	rd, ok := readLane(tx, acc, nil, tx.Issue)
	if !ok {
		engine.Drop(tx)
		return nil
	}
	if prev := engine.Get[CallerPlan](tx, subject); prev != nil && prev.in.equal(rd.in) {
		engine.Put(tx, prev)
		return nil
	}
	cp := &CallerPlan{Lane: acc, budgetAt: -1, linkedAt: -1}
	if h := acc.Spec.Holder; h != nil {
		cp.Holder = h.Key()
	}
	foldCaller(cp, rd)
	engine.Put(tx, cp)
	return nil
}

func personalOffRefusal(off *PersonalAccount, subject string) *Refusal {
	return &Refusal{Code: CodePersonalAccountOff, Rung: limits.RungLane, SubjectID: subject, Message: off.offMessage()}
}

// foldCaller folds read rungs into cp.
func foldCaller(cp *CallerPlan, rd callerReads) {
	cp.in = rd.in
	for i := range rd.rungs {
		cp.Rungs[i] = rd.rungs[i].rung
	}
	lane, group := cp.counterGroup()
	for i := range cp.Rungs {
		r := &cp.Rungs[i]
		if r.ScopeID == "" && r.SubjectID == "" {
			continue
		}
		switch {
		case rd.rungs[i].disabled && cp.Refused == nil:
			what := string(r.Kind) + " " + r.Name + " is disabled"
			if i == rungLane {
				what = "the holder of account " + r.Account.Meta.Name + " is disabled"
			}
			cp.Refused = rungRefusal(CodeSubjectDisabled, r, "", what)
		case rd.rungs[i].closed && cp.Refused == nil:
			cp.Refused = rungRefusal(CodeAccountClosed, r, "", "account "+r.Account.Meta.Name+" is closed: the membership it stood for has ended")
		}
		if refused := rd.rungs[i].refused; refused != nil {
			if cp.Refused == nil {
				cp.Refused = refused
			}
			continue
		}
		if r.Policy != nil {
			cp.foldRung(i, lane, group)
		}
	}
	cp.PayloadLogging = cp.Lane.Spec.PayloadLogging
	for i := len(cp.Rungs) - 1; i >= 0 && cp.PayloadLogging == ""; i-- {
		if a := cp.Rungs[i].Account; a != nil {
			cp.PayloadLogging = a.Spec.PayloadLogging
		}
	}
	cp.Pools = forHolder(callerPools(cp, &rd), cp.Holder)
	// One table per plan: the keys of a lane share it; lanes with the same rungs fill their own.
	cp.Routes = newRouteTable(cp, rd.idx)
}

// foldRung adds rung i's grant and counters. A shared link counts once for the bound account, per (rule, policy, account); one with shared off once per lane under it.
func (cp *CallerPlan) foldRung(i int, lane, group string) {
	r := &cp.Rungs[i]
	ep := r.Policy
	cp.Allow.grants[i] = ep.Grant
	for j := range ep.RateLimits {
		l := &ep.RateLimits[j]
		cs := rateLimitCounters(r, l, lane, group)
		if l.Scoped {
			cp.scoped = append(cp.scoped, scopedCounters{scope: l, counters: cs})
		} else {
			cp.Counters = append(cp.Counters, cs...)
		}
	}
	for j := range ep.Budgets {
		cp.Counters = append(cp.Counters, budgetCounter(r, &ep.Budgets[j], lane, group))
		if cp.budgetAt < 0 {
			cp.budgetAt = i
		}
	}
}

// callerPools resolves the pools the lane reaches. Pools the policy links but that are missing or cannot serve narrow the list; the policy carries the ref_missing issue.
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
		for _, reach := range rd.owned {
			pools = append(pools, reach...)
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

// linkedPools reads the pools the policy links, leaving out the pools, credentials and accounts in unusable (sorted ids the account's scope may not use).
func linkedPools[R *engine.Txn | *engine.View](r R, ep *ExpandedPolicy, unusable []string, in *inputs) []*PoolPlan {
	skip := func(id string) bool {
		_, found := slices.BinarySearch(unusable, id)
		return found
	}
	skipSlot := func(s *KeySlot) bool { return skip(s.Credential.Meta.ID) || skip(s.drawsOn()) }
	var out []*PoolPlan
	for _, pid := range ep.Policy.Spec.PoolIDs {
		if skip(pid) {
			continue
		}
		pp := engine.Get[PoolPlan](r, pid)
		*in = append(*in, pp)
		if pp != nil && len(unusable) > 0 {
			pp = withoutSlots(pp, skipSlot)
		}
		if pp != nil {
			out = append(out, pp)
		}
	}
	return out
}

// withoutSlots returns p without the slots skip names, p itself when none is, nil when none is left.
func withoutSlots(p *PoolPlan, skip func(*KeySlot) bool) *PoolPlan {
	kept := make([]*KeySlot, 0, len(p.Slots))
	for _, s := range p.Slots {
		if !skip(s) {
			kept = append(kept, s)
		}
	}
	switch len(kept) {
	case len(p.Slots):
		return p
	case 0:
		return nil
	}
	cp := *p
	cp.Slots = kept
	return &cp
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

// refusedPlan is a lane that serves nothing, for a key whose lane cannot be used.
func refusedPlan(lane *account.Account, r *Refusal) *CallerPlan {
	cp := &CallerPlan{Lane: lane, Refused: r, budgetAt: -1, linkedAt: -1}
	cp.Routes = newRouteTable(cp, nil)
	return cp
}
