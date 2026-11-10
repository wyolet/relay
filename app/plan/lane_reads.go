// The reads a caller fold makes, generic over a reconcile's Txn and a published View: a reconcile builds the lane plans the request path reads, and a reach read (reach.go) folds a lane from the published plans, optionally with one rung's policy replaced, through the same code.

package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// readLane reads what the caller fold of acc needs: its rungs with their policies and linked pools, and what its scopes own when no rung links. A team's or project's own account reads the rungs above and including it with no lane under them, as a preview of a policy owned there does. over, when set, stands in for the policy bound to one rung's account. issue receives the issues a reconcile files. ok is false when the account heads no ladder: its holder or scope is gone, or it is no caller or provider account.
func readLane[R *engine.Txn | *engine.View](r R, acc *account.Account, over *rungOverride, issue func(engine.Issue)) (rd callerReads, ok bool) {
	subject := accountSubject(acc.Meta.ID)
	rd = newCallerReads(inputs{acc})
	var scopes []string // owner scopes whose credentials the lane reaches without a linking rung, innermost first
	switch acc.DerivedKind() {
	case account.KindProvider:
		scopes = []string{subject}
	case account.KindPersonal:
		if !readHolder(r, &rd, acc) {
			return rd, false
		}
		scopes = []string{ownerRef(acc.Meta.Owner), "system:"}
	case account.KindMember, account.KindService, account.KindTeam, account.KindProject:
		if h := acc.Spec.Holder; h != nil && !readHolder(r, &rd, acc) || !readScopes(r, &rd, acc, issue) {
			return rd, false
		}
		if p := rd.rungs[rungProject].rung.ScopeID; p != "" {
			scopes = append(scopes, "project:"+p)
		}
		scopes = append(scopes, "team:"+rd.rungs[rungTeam].rung.ScopeID, "system:")
	default:
		return rd, false
	}
	if !acc.IsProvider() {
		if acc.Spec.Holder != nil {
			lane := &rd.rungs[rungLane]
			lane.rung.SubjectID, lane.rung.Account, lane.closed = subject, acc, acc.Spec.Closed
			lane.rung.ScopeID = rd.rungs[rungProject].rung.ScopeID
			if lane.rung.ScopeID == "" {
				lane.rung.ScopeID = rd.rungs[rungTeam].rung.ScopeID
			}
		}
		for i := range rd.rungs {
			readRung(r, &rd.rungs[i], &rd.in, over)
		}
	}
	readReach(r, &rd, acc, scopes)
	return rd, true
}

func newCallerReads(in inputs) callerReads {
	return callerReads{in: in, rungs: [3]rungRead{{rung: Rung{Kind: limits.RungTeam}}, {rung: Rung{Kind: limits.RungProject}}, {rung: Rung{Kind: limits.RungLane}}}}
}

// readReach reads what the lane reaches when no rung links pools, and the catalog.
func readReach[R *engine.Txn | *engine.View](r R, rd *callerReads, acc *account.Account, scopes []string) {
	if !rd.links() {
		if acc != nil && acc.IsShared() {
			// A shared account used as a lane reaches the keys of its real account.
			rd.owned = append(rd.owned, readSharedLane(r, acc.Meta.ID, &rd.in))
		}
		for _, s := range scopes {
			rd.owned = append(rd.owned, readOwned(r, s, &rd.in))
		}
	}
	rd.idx = engine.Get[CatalogIndex](r, "")
	rd.in = append(rd.in, rd.idx)
}

// readHolder reads the user or service account acting in the lane; false when it is gone.
func readHolder[R *engine.Txn | *engine.View](r R, rd *callerReads, acc *account.Account) bool {
	h := acc.Spec.Holder
	lane := &rd.rungs[rungLane]
	switch h.Kind {
	case account.HolderUser:
		u := engine.Get[user.User](r, h.ID)
		if u == nil {
			return false
		}
		// The user row also carries login state; only the disabled flag shapes the plan.
		lane.disabled = u.Disabled
		rd.in = append(rd.in, u.ID, u.Disabled)
	case account.HolderServiceAccount:
		sa := engine.Get[serviceaccount.ServiceAccount](r, h.ID)
		if sa == nil {
			return false
		}
		lane.rung.Name, lane.disabled = sa.Meta.Name, !sa.IsEnabled()
		rd.in = append(rd.in, sa)
	}
	return true
}

// readScopes reads the team and project the account sits in, and their own accounts; false when the scope is gone.
func readScopes[R *engine.Txn | *engine.View](r R, rd *callerReads, lane *account.Account, issue func(engine.Issue)) bool {
	scope := lane.Meta.Owner
	teamID := scope.ID
	if scope.Kind == meta.OwnerProject {
		p := engine.Get[project.Project](r, scope.ID)
		if p == nil {
			issue(parentMissing(engine.Ref{Kind: KindAccount, ID: lane.Meta.ID}, "metadata.owner", engine.Ref{Kind: KindProject, ID: scope.ID}))
			return false
		}
		rd.rungs[rungProject] = scopeRung(r, limits.RungProject, "project:"+p.Meta.ID, p.Meta.ID, p.Meta.Name, !p.IsEnabled())
		rd.in = append(rd.in, p)
		teamID = p.Spec.TeamID
	}
	t := engine.Get[team.Team](r, teamID)
	if t == nil {
		issue(parentMissing(engine.Ref{Kind: KindProject, ID: scope.ID}, "spec.teamId", engine.Ref{Kind: KindTeam, ID: teamID}))
		return false
	}
	rd.rungs[rungTeam] = scopeRung(r, limits.RungTeam, "team:"+t.Meta.ID, t.Meta.ID, t.Meta.Name, !t.IsEnabled())
	rd.in = append(rd.in, t)
	for _, i := range []int{rungTeam, rungProject} {
		if a := rd.rungs[i].rung.Account; a != nil {
			rd.in = append(rd.in, a)
		}
	}
	return true
}

// scopeRung is a team's or project's rung: its own account, when relay has made it, carries the policy.
func scopeRung[R *engine.Txn | *engine.View](r R, kind limits.Rung, managedKey, scopeID, name string, disabled bool) rungRead {
	rr := rungRead{rung: Rung{Kind: kind, ScopeID: scopeID, Name: name}, disabled: disabled}
	if m := engine.Get[ManagedAccount](r, managedKey); m != nil && m.Row != nil {
		rr.rung.SubjectID, rr.rung.Account = accountSubject(m.Row.Meta.ID), m.Row
	}
	return rr
}

// rungOverride stands in for the policy bound to one account (subject, "account:<id>"): a preview's draft, or a saved policy read as if bound there.
type rungOverride struct {
	subject string
	policy  *ExpandedPolicy
	// unusable are the links the account's scope may not use, sorted, as a binding's would be.
	unusable []string
}

// readRung reads the policy bound to the rung's account, or over's when it stands in for that account, and, when the policy links pools, what it links.
func readRung[R *engine.Txn | *engine.View](r R, rr *rungRead, in *inputs, over *rungOverride) {
	if over != nil && over.subject != "" && rr.rung.SubjectID == over.subject {
		applyOverride(r, rr, over, in)
		return
	}
	if rr.rung.SubjectID == "" {
		return
	}
	rp := engine.Get[RungPolicy](r, rr.rung.SubjectID)
	*in = append(*in, rp)
	if rp == nil {
		return
	}
	rr.rung.Policy, rr.refused = rp.Rung.Policy, rp.Refused
	if rr.rung.Policy.links() {
		rr.linked = linkedPools(r, rr.rung.Policy, rp.unusable, in)
	}
}

// applyOverride puts over's policy on the rung, read as a binding of it would be.
func applyOverride[R *engine.Txn | *engine.View](r R, rr *rungRead, over *rungOverride, in *inputs) {
	ep := over.policy
	rr.rung.Policy = ep
	if !ep.Policy.IsEnabled() {
		rr.refused = rungRefusal(CodePolicyDisabled, &rr.rung, ep.Policy.Meta.ID, "policy "+ep.Policy.Meta.Name+" is disabled")
	}
	if ep.links() {
		rr.linked = linkedPools(r, ep, over.unusable, in)
	}
}

// readOwned is what scope reaches by ownership: its own keys. Accounts shared with it are reached only through a pool that lists them, so a share never changes routing on its own.
func readOwned[R *engine.Txn | *engine.View](r R, scope string, in *inputs) []*PoolPlan {
	oc := engine.Get[OwnedCredentials](r, scope)
	*in = append(*in, oc)
	if oc == nil {
		return nil
	}
	return oc.Pools
}

// readSharedLane is what a shared account used as a lane reaches: its real account's keys, narrowed by its own policy.
func readSharedLane[R *engine.Txn | *engine.View](r R, id string, in *inputs) []*PoolPlan {
	sa := engine.Get[SharedAccount](r, id)
	*in = append(*in, sa)
	if sa == nil {
		return nil
	}
	out := make([]*PoolPlan, len(sa.Slots))
	for i, s := range sa.Slots {
		out[i] = s.Alone
	}
	return out
}
