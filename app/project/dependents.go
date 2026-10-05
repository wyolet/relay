package project

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
)

// Rows are the rows that can live under a project. Deleting the project
// would cascade through the foreign keys to its service accounts, their
// keys and its policy bindings, and leave its policies, host keys and rate
// limits owned by a project that no longer exists — so apply's prune
// refuses while any remain.
type Rows struct {
	ServiceAccounts []*serviceaccount.ServiceAccount
	Keys            []*key.Key
	Policies        []*policy.Policy
	HostKeys        []*hostkey.HostKey
	RateLimits      []*ratelimit.RateLimit
	PolicyBindings  []*policybinding.PolicyBinding
}

// Gone reports a row the caller is deleting in the same operation, keyed by
// manifest kind and id. Such a row is not in the way.
type Gone func(kind, id string) bool

// Visible reports a row the caller may see, keyed by manifest kind. Nil sees
// every row.
type Visible func(kind string, m meta.Metadata) bool

// Dependents names, as "Kind/name", every row in rows under project id the
// caller may see, and counts the rest.
func (r Rows) Dependents(id string, gone Gone, visible Visible) (named []string, hidden int) {
	add := func(kind string, m meta.Metadata, under bool) {
		switch {
		case !under || (gone != nil && gone(kind, m.ID)):
		case visible != nil && !visible(kind, m):
			hidden++
		default:
			named = append(named, kind+"/"+m.Name)
		}
	}
	owned := meta.Owner{Kind: meta.OwnerProject, ID: id}
	for _, x := range r.ServiceAccounts {
		add("ServiceAccount", x.Meta, x.Spec.ProjectID == id)
	}
	for _, x := range r.Keys {
		add("Key", x.Meta, x.Meta.Owner == owned)
	}
	for _, x := range r.Policies {
		add("Policy", x.Meta, x.Meta.Owner == owned)
	}
	for _, x := range r.HostKeys {
		add("HostKey", x.Meta, x.Meta.Owner == owned)
	}
	for _, x := range r.RateLimits {
		add("RateLimit", x.Meta, x.Meta.Owner == owned)
	}
	for _, x := range r.PolicyBindings {
		add("PolicyBinding", x.Meta, x.Spec.ProjectID == id)
	}
	return named, hidden
}

// OfTeam names, as "Project/name", every project in projects under team id
// the caller may see, and counts the rest.
func OfTeam(teamID string, projects []*Project, gone Gone, visible Visible) (named []string, hidden int) {
	for _, p := range projects {
		switch {
		case p.Spec.TeamID != teamID || (gone != nil && gone("Project", p.Meta.ID)):
		case visible != nil && !visible("Project", p.Meta):
			hidden++
		default:
			named = append(named, "Project/"+p.Meta.Name)
		}
	}
	return named, hidden
}

// DependentsError refuses deleting a team or project that still has rows
// under it. The caller names the team or project. Hidden rows still block
// but are only counted.
type DependentsError struct {
	Rows   []string
	Hidden int
}

// maxNamedDependents bounds the error: a project can hold thousands of keys.
const maxNamedDependents = 10

func (e *DependentsError) Error() string {
	named, more := e.Rows, e.Hidden
	if len(named) > maxNamedDependents {
		more += len(named) - maxNamedDependents
		named = named[:maxNamedDependents]
	}
	if len(named) == 0 {
		return fmt.Sprintf("still has %d row(s) under it: delete or move them first", more)
	}
	tail := ""
	if more > 0 {
		tail = fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("still has %s%s: delete or move them first", strings.Join(named, ", "), tail)
}
