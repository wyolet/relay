package plan

import (
	"github.com/wyolet/relay/app/project"
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

// ProjectIndex finds an enabled project by id, for the project a token is scoped to.
type ProjectIndex struct{ byID map[string]*project.Project }

// Lookup returns project id, nil when it does not exist or is disabled.
func (x *ProjectIndex) Lookup(id string) *project.Project {
	if x == nil {
		return nil
	}
	return x.byID[id]
}

func (c *controllers) projectIndex(tx *engine.Txn, _ string) error {
	ps := engine.Members[project.Project](tx)
	x := &ProjectIndex{byID: make(map[string]*project.Project, len(ps))}
	for _, p := range ps {
		if p.IsEnabled() {
			x.byID[p.Meta.ID] = p
		}
	}
	engine.Put(tx, x)
	return nil
}
