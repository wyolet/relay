// What a change on a team or project does to the lanes under it, answered by the access and limits kinds: a change there applies to everything under the target, so it is reported, never refused, from the same comparison of the plans with and without the rows as the check on other accounts. A lane is counted by the (model, host) pairs it is served at, and named by who holds it.

package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// underListed caps the lanes PseudoUnder names.
const underListed = 20

// PseudoUnder is what the rows do to the lanes under a team or project target.
type PseudoUnder struct {
	Lanes             int         `json:"lanes"                       doc:"Lanes under the target: those of its projects, service accounts and members, those the caller may not see included."`
	Narrowed          int         `json:"narrowed"                    doc:"Lanes under it that lose at least one model at a host they are served at and gain none. Another pool or account serving the same model at the same host is no change."`
	Widened           int         `json:"widened"                     doc:"Lanes under it that gain at least one model at a host and lose none."`
	Changed           int         `json:"changed"                     doc:"Lanes under it that both lose and gain one. narrowed, widened and changed count each lane once at most."`
	Unreachable       []UnderLane `json:"unreachable"                 doc:"Lanes under it whose holder the caller may see that were served at some model and host before and at none after, at most 20."`
	UnreachableMore   int         `json:"unreachableMore,omitempty"   doc:"Lanes the caller may see that become unreachable beyond those listed."`
	UnreachableHidden int         `json:"unreachableHidden,omitempty" doc:"Lanes whose holder the caller may not see that become unreachable: counted, not named."`
}

// UnderLane is the holder of one lane under the target.
type UnderLane struct {
	Kind string `json:"kind" enum:"service-account,user,project,team" doc:"Who holds the lane: a service account, a user (a member's lane), or a project or team for its own account."`
	ID   string `json:"id"   doc:"Id of the holder."`
	Name string `json:"name" doc:"The service account's, project's or team's display name (its slug when it has none), or the user's username."`
}

// inSubtree reports whether cp is the lane of targetID or climbs it: a project or lane under a team, a lane under a project.
func inSubtree(targetID string) func(*plan.CallerPlan) bool {
	return func(cp *plan.CallerPlan) bool {
		if cp.Lane != nil && cp.Lane.Meta.ID == targetID {
			return true
		}
		for _, r := range cp.Rungs {
			if r.Account != nil && r.Account.Meta.ID == targetID {
				return true
			}
		}
		return false
	}
}

// under answers what rows do to the lanes under target, as f found them; nil for a target that is no team or project, and when rows write nothing.
func (d Deps) under(ctx context.Context, f plan.Findings, target *account.Account, rows []pseudo.Row) (*PseudoUnder, error) {
	if k := target.DerivedKind(); len(rows) == 0 || k != account.KindTeam && k != account.KindProject {
		return nil, nil
	}
	base, err := f.Baseline()
	if err != nil {
		return nil, err
	}
	id := target.Meta.ID
	sub := inSubtree(id)
	var lanes int
	var changed []plan.LaneReachChange
	if err := f.Within(func(ctx context.Context) error {
		lanes, changed, err = plan.LanesUnder(ctx, base, f.Result, func(cp *plan.CallerPlan) bool { return cp.Lane.Meta.ID != id && sub(cp) })
		return err
	}); err != nil {
		return nil, err
	}
	out := &PseudoUnder{Lanes: lanes, Unreachable: []UnderLane{}}
	for _, c := range changed {
		switch {
		case c.Lost && c.Gained:
			out.Changed++
		case c.Lost:
			out.Narrowed++
		default:
			out.Widened++
		}
		if !c.Unreachable {
			continue
		}
		holder, seen := d.laneHolder(ctx, f.Result, c.AccountID)
		switch {
		case !seen:
			out.UnreachableHidden++
		case len(out.Unreachable) == underListed:
			out.UnreachableMore++
		default:
			out.Unreachable = append(out.Unreachable, holder)
		}
	}
	return out, nil
}

// laneHolder is who holds the lane accountID in v, and whether the caller may see that holder: a service account or a user for a lane, a project or team for its own account.
func (d Deps) laneHolder(ctx context.Context, v *engine.View, accountID string) (UnderLane, bool) {
	a := engine.Get[account.Account](v, accountID)
	if a == nil {
		return UnderLane{}, false
	}
	if h := a.Spec.Holder; h != nil {
		switch h.Kind {
		case account.HolderServiceAccount:
			sa := engine.Get[serviceaccount.ServiceAccount](v, h.ID)
			if sa == nil || !visibleTo(ctx, d.Authz, "service-account", sa.Meta.ID, sa.Meta.Owner) {
				return UnderLane{}, false
			}
			return UnderLane{Kind: "service-account", ID: sa.Meta.ID, Name: label(sa.Meta)}, true
		case account.HolderUser:
			if !d.userVisible(ctx, h.ID) {
				return UnderLane{}, false
			}
			return UnderLane{Kind: "user", ID: h.ID, Name: d.ownerName(ctx, meta.Owner{Kind: meta.OwnerUser, ID: h.ID})}, true
		}
		return UnderLane{}, false
	}
	switch o := a.Meta.Owner; o.Kind {
	case meta.OwnerProject:
		if p := engine.Get[project.Project](v, o.ID); p != nil && visibleTo(ctx, d.Authz, "project", p.Meta.ID, p.Meta.Owner) {
			return UnderLane{Kind: "project", ID: p.Meta.ID, Name: label(p.Meta)}, true
		}
	case meta.OwnerTeam:
		if t := engine.Get[team.Team](v, o.ID); t != nil && visibleTo(ctx, d.Authz, "team", t.Meta.ID, t.Meta.Owner) {
			return UnderLane{Kind: "team", ID: t.Meta.ID, Name: label(t.Meta)}, true
		}
	}
	return UnderLane{}, false
}
