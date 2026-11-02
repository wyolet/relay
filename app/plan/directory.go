package plan

import (
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// UserPlan is what a relay-minted token of one user authenticates against: the version every token of the user must carry.
type UserPlan struct {
	UserID       string
	TokenVersion int
	in           inputs
}

func (u *UserPlan) builtFrom() inputs { return u.in }

func (c *controllers) userPlan(tx *engine.Txn, id string) error {
	u := engine.Get[user.User](tx, id)
	if u == nil {
		engine.Drop(tx)
		return nil
	}
	up := &UserPlan{UserID: id, TokenVersion: u.TokenVersion, in: inputs{u.TokenVersion}}
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
