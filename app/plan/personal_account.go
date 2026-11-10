// A user's personal account can be turned off by the deployment, by a group the user is in, or for the user alone. The effective state is folded per user from those rows here, so the request path and the control reads see one answer without asking Postgres. Off closes the user's personal lane and the provider accounts the user owns personally; their rows stay.

package plan

import (
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// PersonalAccountSource names what decided a user's personal-account state.
type PersonalAccountSource string

const (
	PersonalAccountFromUser    PersonalAccountSource = "user"
	PersonalAccountFromGroup   PersonalAccountSource = "group"
	PersonalAccountFromSetting PersonalAccountSource = "setting"
)

// PersonalAccount is a user's effective personal-account state.
type PersonalAccount struct {
	UserID   string
	Username string
	On       bool
	Source   PersonalAccountSource
	// GroupID is the group that decided it, for Source group.
	GroupID string
	in      inputs
}

func (p *PersonalAccount) builtFrom() inputs { return p.in }

// EffectivePersonalAccount is the first of: the user's own value; an enabled system group's, off winning over on and the lowest id deciding among equals; the deployment's setting. A nil setting is on.
func EffectivePersonalAccount(userValue string, groups []*group.Group, setting *settings.Accounts) (on bool, source PersonalAccountSource, groupID string) {
	if userValue != "" {
		return userValue != settings.PersonalAccountsOff, PersonalAccountFromUser, ""
	}
	var onGroup, offGroup string
	for _, g := range groups {
		// Only system groups may carry a value; one on a user's group, written before that rule, says nothing.
		if !g.IsEnabled() || g.Meta.Owner.Kind != meta.OwnerSystem {
			continue
		}
		switch g.Spec.PersonalAccount {
		case settings.PersonalAccountsOff:
			if offGroup == "" || g.Meta.ID < offGroup {
				offGroup = g.Meta.ID
			}
		case settings.PersonalAccountsOn:
			if onGroup == "" || g.Meta.ID < onGroup {
				onGroup = g.Meta.ID
			}
		}
	}
	switch {
	case offGroup != "":
		return false, PersonalAccountFromGroup, offGroup
	case onGroup != "":
		return true, PersonalAccountFromGroup, onGroup
	}
	return setting.PersonalAccountsAreOn(), PersonalAccountFromSetting, ""
}

// groupMembers is a group's Watch key into the personal-account states: every member's.
func groupMembers(g *group.Group) []string { return g.Spec.MemberIDs }

func (c *controllers) personalAccount(tx *engine.Txn, userID string) error {
	u := engine.Get[user.User](tx, userID)
	if u == nil {
		engine.Drop(tx)
		return nil
	}
	setting := engine.Get[settings.Accounts](tx, settings.SectionAccounts)
	on, source, groupID := EffectivePersonalAccount(u.PersonalAccount, engine.Members[group.Group](tx), setting)
	p := &PersonalAccount{UserID: u.ID, Username: u.Username, On: on, Source: source, GroupID: groupID}
	p.in = inputs{u.ID, u.Username, on, source, groupID}
	engine.Put(tx, SameOr(engine.Get[PersonalAccount](tx, userID), p))
	return nil
}

// personalAccountOff returns the state of a user owner whose personal account is off, nil for any other owner. Reading it records the dependency, so the caller rebuilds when it turns.
func personalAccountOff[R *engine.Txn | *engine.View](r R, owner meta.Owner) *PersonalAccount {
	if owner.Kind != meta.OwnerUser || owner.ID == "" {
		return nil
	}
	if p := engine.Get[PersonalAccount](r, owner.ID); p != nil && !p.On {
		return p
	}
	return nil
}

// PersonalAccountOf returns userID's personal-account state in view v; nil when the user is unknown.
func PersonalAccountOf(v *engine.View, userID string) *PersonalAccount {
	return engine.Get[PersonalAccount](v, userID)
}

// offMessage is the refusal wording for a user whose personal account is off.
func (p *PersonalAccount) offMessage() string {
	return "personal accounts are turned off for " + p.name() + "; use a team or project key"
}

func (p *PersonalAccount) name() string {
	if p.Username != "" {
		return p.Username
	}
	return "user " + p.UserID
}
