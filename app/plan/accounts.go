// Caller accounts are rows relay keeps in step with the tenancy rows: a team's and a project's own account, a personal account per user, an account per service account, and a member account per user and team or project a role binding names the user in. These controllers work out, per account place, what should exist and what does; a writer off the engine (PendingAccounts) makes the rows, since a reconcile does no I/O. Deleting a team, project, user or service account drops its accounts through the foreign keys, so only creating, closing and reopening is left to that writer.

package plan

import (
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// AccountAction is what a ManagedAccount needs done to its row.
type AccountAction uint8

const (
	AccountUnchanged AccountAction = iota
	// AccountCreate makes the row Want describes.
	AccountCreate
	// AccountClose closes a member account whose last role grant went.
	AccountClose
	// AccountReopen opens a member account a role grants again, and marks it as following role bindings.
	AccountReopen
)

// ManagedAccount is one caller account's place (account.Account.ManagedKey): the account its source row calls for, and the row holding the place, if any.
type ManagedAccount struct {
	Key string
	// Want is the account to create when Row is nil; nil when nothing calls for one (the source row is gone, or no role grants the membership).
	Want *account.Account
	Row  *account.Account
	// member and granted describe a member account's place: whether a role binding names the user in the scope.
	member, granted bool
	in              inputs
}

func (m *ManagedAccount) builtFrom() inputs { return m.in }

// Action is what the place needs done.
func (m *ManagedAccount) Action() AccountAction {
	switch {
	case m.Row == nil && m.Want != nil:
		return AccountCreate
	case m.Row == nil || !m.member:
		return AccountUnchanged
	case m.granted && (m.Row.Spec.Closed || !m.Row.FollowsRoleBindings):
		return AccountReopen
	case !m.granted && m.Row.FollowsRoleBindings && !m.Row.Spec.Closed:
		return AccountClose
	}
	return AccountUnchanged
}

// managedKeyOf is an account's Watch key: its place, none for a provider account.
func managedKeyOf(a *account.Account) []string {
	if k := a.ManagedKey(); k != "" {
		return []string{k}
	}
	return nil
}

// memberKeys is a role binding's Watch key: the member account place of every user it names directly in a team or project scope.
func memberKeys(b *rolebinding.RoleBinding) []string {
	scope := b.Spec.Scope
	if scope.Kind != meta.OwnerTeam && scope.Kind != meta.OwnerProject {
		return nil
	}
	var out []string
	for _, s := range b.Spec.Subjects {
		if s.Kind == rolebinding.SubjectUser && s.ID != "" {
			out = append(out, account.MemberKey(scope, s.ID))
		}
	}
	return out
}

func (c *controllers) managedAccount(tx *engine.Txn, key string) error {
	m := &ManagedAccount{Key: key}
	for _, a := range engine.Members[account.Account](tx) {
		m.Row = a
	}
	kind, rest, _ := strings.Cut(key, ":")
	switch kind {
	case "team":
		if t := engine.Get[team.Team](tx, rest); t != nil {
			m.Want = account.ForTeam(t.Meta.ID, t.Meta.Name, t.Meta.DisplayName)
		}
	case "project":
		if p := engine.Get[project.Project](tx, rest); p != nil {
			m.Want = account.ForProject(p.Meta.ID, p.Meta.Name, p.Meta.DisplayName)
		}
	case "personal":
		if u := engine.Get[user.User](tx, rest); u != nil {
			m.Want = account.Personal(u.ID, u.Username)
		}
	case "service":
		if sa := engine.Get[serviceaccount.ServiceAccount](tx, rest); sa != nil {
			m.Want = account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Spec.ProjectID)
		}
	case "member":
		m.member = true
		m.Want, m.granted = wantMember(tx, rest)
	}
	if m.Row == nil && m.Want == nil {
		engine.Drop(tx)
		return nil
	}
	// The wanted row's names are read once, at create; a rename of its source rebuilds nothing.
	m.in = inputs{m.Row, m.Want != nil, m.granted}
	engine.Put(tx, SameOr(engine.Get[ManagedAccount](tx, key), m))
	return nil
}

// wantMember is the member account a place "<team|project>:<scope id>:<user id>" calls for and whether an enabled role binding naming the user in that scope grants it; the account is nil without a grant, or when the user or scope is gone.
func wantMember(tx *engine.Txn, place string) (*account.Account, bool) {
	parts := strings.SplitN(place, ":", 3)
	if len(parts) != 3 {
		return nil, false
	}
	scope := meta.Owner{Kind: meta.OwnerKind(parts[0]), ID: parts[1]}
	granted := slices.ContainsFunc(engine.Members[rolebinding.RoleBinding](tx), func(b *rolebinding.RoleBinding) bool {
		return b.IsEnabled() && b.Spec.Scope == scope
	})
	u := engine.Get[user.User](tx, parts[2])
	if !granted || u == nil {
		return nil, granted
	}
	var name, display string
	switch scope.Kind {
	case meta.OwnerTeam:
		t := engine.Get[team.Team](tx, scope.ID)
		if t == nil {
			return nil, granted
		}
		name, display = t.Meta.Name, t.Meta.DisplayName
	case meta.OwnerProject:
		p := engine.Get[project.Project](tx, scope.ID)
		if p == nil {
			return nil, granted
		}
		name, display = p.Meta.Name, p.Meta.DisplayName
	default:
		return nil, false
	}
	a := account.Member(u.ID, u.Username, scope, name, display)
	a.FollowsRoleBindings = true
	return a, granted
}

// PendingAccounts is every account place whose row needs creating, closing or reopening, sorted by key: the work list of the writer off the engine.
type PendingAccounts struct {
	Items []*ManagedAccount
}

// pendingKey is a ManagedAccount's Watch key into the pending list: the singleton while it needs work.
func pendingKey(m *ManagedAccount) []string {
	if m.Action() == AccountUnchanged {
		return nil
	}
	return singleton
}

func (c *controllers) pendingAccounts(tx *engine.Txn, _ string) error {
	items := engine.Members[ManagedAccount](tx)
	if len(items) == 0 {
		engine.Drop(tx)
		return nil
	}
	slices.SortFunc(items, func(a, b *ManagedAccount) int { return strings.Compare(a.Key, b.Key) })
	engine.Put(tx, &PendingAccounts{Items: items})
	return nil
}

// Pending returns the account places that need work in view v.
func Pending(v *engine.View) []*ManagedAccount {
	if p := engine.Get[PendingAccounts](v, ""); p != nil {
		return p.Items
	}
	return nil
}
