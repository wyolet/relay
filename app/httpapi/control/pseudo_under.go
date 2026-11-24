// What a change on a team or project does to the lanes under it, answered by the access and limits kinds: a change there applies to everything under the target, so it is reported, never refused, from the same comparison of the plans with and without the rows as the check on other accounts.

package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/engine"
)

// underListed caps the lanes PseudoUnder names.
const underListed = 20

// PseudoUnder is what the rows do to the lanes under a team or project target.
type PseudoUnder struct {
	Lanes           int         `json:"lanes"                     doc:"Lanes under the target: those of its projects, service accounts and members, those the caller may not see included."`
	Narrowed        int         `json:"narrowed"                  doc:"Lanes under it that lose at least one model, host, or pool or account a model comes through."`
	Widened         int         `json:"widened"                   doc:"Lanes under it that gain at least one."`
	Unreachable     []UnderLane `json:"unreachable"               doc:"Lanes under it that served a model at some host before and serve none after, at most 20; a lane the caller may not see has no name."`
	UnreachableMore int         `json:"unreachableMore,omitempty" doc:"Lanes that become unreachable beyond those listed."`
}

// UnderLane is one lane under the target.
type UnderLane struct {
	Kind string `json:"kind"           doc:"account: a lane is an account."`
	ID   string `json:"id"             doc:"Id of the lane's account."`
	Name string `json:"name,omitempty" doc:"Slug of the lane's account; omitted when the caller may not see it."`
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
		if c.Narrowed {
			out.Narrowed++
		}
		if c.Widened {
			out.Widened++
		}
		if !c.Unreachable {
			continue
		}
		if len(out.Unreachable) == underListed {
			out.UnreachableMore++
			continue
		}
		lane := UnderLane{Kind: "account", ID: c.AccountID}
		if a := engine.Get[account.Account](f.Result, c.AccountID); a != nil && visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			lane.Name = a.Meta.Name
		}
		out.Unreachable = append(out.Unreachable, lane)
	}
	return out, nil
}
