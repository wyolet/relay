package plan

import (
	"maps"

	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// UserPlan is what a relay-minted token of one user authenticates against: the version every token of the user must carry and the user's caller fold.
type UserPlan struct {
	UserID       string
	TokenVersion int
	Caller       *CallerPlan
	in           inputs
}

func (u *UserPlan) builtFrom() inputs { return u.in }

func (c *controllers) userPlan(tx *engine.Txn, id string) error {
	u := engine.Get[user.User](tx, id)
	if u == nil {
		engine.Drop(tx)
		return nil
	}
	cp := engine.Get[CallerPlan](tx, "user:"+id)
	if cp == nil {
		engine.Drop(tx)
		return nil
	}
	up := &UserPlan{UserID: id, TokenVersion: u.TokenVersion, Caller: cp, in: inputs{u.TokenVersion, cp}}
	engine.Put(tx, SameOr(engine.Get[UserPlan](tx, id), up))
	return nil
}

// UserIndex finds a user's plan by user id, for token authentication.
type UserIndex struct{ byID map[string]*UserPlan }

// Lookup returns the plan of user id, nil when the user has none.
func (x *UserIndex) Lookup(id string) *UserPlan {
	if x == nil {
		return nil
	}
	return x.byID[id]
}

func (c *controllers) userIndex(tx *engine.Txn, _ string) error {
	ups := engine.Members[UserPlan](tx)
	x := &UserIndex{byID: make(map[string]*UserPlan, len(ups))}
	for _, up := range ups {
		x.byID[up.UserID] = up
	}
	engine.Put(tx, x)
	return nil
}

// ProjectPlan is what a token scoped to an enabled project adds to its user's plan: the project's and its team's rungs, read once.
type ProjectPlan struct {
	Project *project.Project
	rungs   [2]rungRead          // team, project
	owned   [2]*OwnedCredentials // project's, team's
	in      inputs
}

// projectCaller is a user's plan in a project, with the project plan it was folded from.
type projectCaller struct {
	project *ProjectPlan
	caller  *CallerPlan
}

// maxProjectCallers bounds the projects one user plan keeps a folded plan for; past it the cache starts over.
const maxProjectCallers = 128

func (p *ProjectPlan) builtFrom() inputs { return p.in }

func (c *controllers) projectPlan(tx *engine.Txn, id string) error {
	proj := engine.Get[project.Project](tx, id)
	if proj == nil || !proj.IsEnabled() {
		engine.Drop(tx)
		return nil
	}
	tm := engine.Get[team.Team](tx, proj.Spec.TeamID)
	if tm == nil {
		engine.Drop(tx)
		return nil
	}
	pp := &ProjectPlan{Project: proj, rungs: [2]rungRead{teamRung(tm), projectRung(proj)}, in: inputs{proj, tm}}
	for i := range pp.rungs {
		readRung(tx, &pp.rungs[i], &pp.in)
	}
	// Read always: whether the project's and team's credentials are reached depends on the user's rung, known only per token.
	pp.owned = [2]*OwnedCredentials{readOwned(tx, "project:"+id, &pp.in), readOwned(tx, "team:"+tm.Meta.ID, &pp.in)}
	engine.Put(tx, SameOr(engine.Get[ProjectPlan](tx, id), pp))
	return nil
}

// CallerFor returns the plan of the user whose own plan is user, acting in this project: the user's rung under the project's and its team's, reaching what the project and the team own as well as what the user does. The user's rung is the binding that governs inside this project: a system-owned one, or one the project or its team owns. Folded on first use and kept on the user's plan while this project plan is current; the cache goes with the user's plan, so a rebuilt plan (any catalog change rebuilds every one) releases the old folds and the catalog they hold.
func (p *ProjectPlan) CallerFor(user *CallerPlan) *CallerPlan {
	id := p.Project.Meta.ID
	cache := user.inProjects
	var cached *map[string]projectCaller
	if cache != nil {
		if cached = cache.m.Load(); cached != nil {
			if e, ok := (*cached)[id]; ok && e.project == p {
				return e.caller
			}
		}
	}
	rd := user.reads
	rd.rungs[rungTeam], rd.rungs[rungProject] = p.rungs[0], p.rungs[1]
	rd.rungs[rungPrincipal] = rd.principalIn(p.rungs[1].rung.SubjectID, p.rungs[0].rung.SubjectID)
	rd.owned[ownedProject], rd.owned[ownedTeam] = p.owned[0], p.owned[1]
	cp := foldCaller(user.Principal, rd)
	if cache == nil {
		return cp
	}
	// Copy on write: readers take no lock. Two first requests racing may both fold; the one stored wins.
	for {
		next := map[string]projectCaller{}
		if cached != nil && len(*cached) < maxProjectCallers {
			if e, ok := (*cached)[id]; ok && e.project == p {
				return e.caller
			}
			maps.Copy(next, *cached)
		}
		next[id] = projectCaller{project: p, caller: cp}
		if cache.m.CompareAndSwap(cached, &next) {
			return cp
		}
		cached = cache.m.Load()
	}
}

// ProjectIndex finds an enabled project's plan by id, for the project a token is scoped to.
type ProjectIndex struct{ byID map[string]*ProjectPlan }

// Lookup returns project id's plan, nil when the project or its team does not exist or the project is disabled.
func (x *ProjectIndex) Lookup(id string) *ProjectPlan {
	if x == nil {
		return nil
	}
	return x.byID[id]
}

func (c *controllers) projectIndex(tx *engine.Txn, _ string) error {
	ps := engine.Members[ProjectPlan](tx)
	x := &ProjectIndex{byID: make(map[string]*ProjectPlan, len(ps))}
	for _, p := range ps {
		x.byID[p.Project.Meta.ID] = p
	}
	engine.Put(tx, x)
	return nil
}
