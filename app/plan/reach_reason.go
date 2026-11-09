package plan

import (
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/engine"
)

// ReasonOf words a refusal for a reader: a model refused by a rung's policy names the rung and the policy by slug, any other refusal is its message.
func ReasonOf(v *engine.View, r *Refusal) string { return refusalReason(v, r) }

func refusalReason[R *engine.Txn | *engine.View](rd R, r *Refusal) string {
	if r.Code != CodeModelNotAllowed || r.PolicyID == "" {
		return r.Message
	}
	p := engine.Get[policy.Policy](rd, r.PolicyID)
	_, accID, _ := strings.Cut(r.SubjectID, ":")
	acc := engine.Get[account.Account](rd, accID)
	if p == nil || acc == nil {
		return r.Message
	}
	if r.Rung == limits.RungAccount {
		return "not allowed by policy " + p.Meta.Name + " on provider account " + acc.Meta.Name
	}
	return "not allowed by " + rungHolder(rd, r.Rung, acc) + "'s policy " + p.Meta.Name
}

// rungHolder names what a caller rung stands for: its team or project by slug, else the account.
func rungHolder[R *engine.Txn | *engine.View](rd R, rung limits.Rung, acc *account.Account) string {
	switch rung {
	case limits.RungTeam:
		if t := engine.Get[team.Team](rd, acc.Meta.Owner.ID); t != nil {
			return "team " + t.Meta.Name
		}
	case limits.RungProject:
		if p := engine.Get[project.Project](rd, acc.Meta.Owner.ID); p != nil {
			return "project " + p.Meta.Name
		}
	}
	return "account " + acc.Meta.Name
}
