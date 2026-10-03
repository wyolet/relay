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
// limits owned by a project that no longer exists — so both delete paths
// refuse while any remain.
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

// Dependents names, as "Kind/name", every row in rows under project id.
func (r Rows) Dependents(id string, gone Gone) []string {
	var out []string
	add := func(kind string, m meta.Metadata, under bool) {
		if under && (gone == nil || !gone(kind, m.ID)) {
			out = append(out, kind+"/"+m.Name)
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
	return out
}

// OfTeam names, as "Project/name", every project in projects under team id.
func OfTeam(teamID string, projects []*Project, gone Gone) []string {
	var out []string
	for _, p := range projects {
		if p.Spec.TeamID == teamID && (gone == nil || !gone("Project", p.Meta.ID)) {
			out = append(out, "Project/"+p.Meta.Name)
		}
	}
	return out
}

// DependentsError refuses deleting a team or project that still has rows
// under it. The caller names the team or project.
type DependentsError struct {
	Rows []string
}

// maxNamedDependents bounds the error: a project can hold thousands of keys.
const maxNamedDependents = 10

func (e *DependentsError) Error() string {
	named := e.Rows
	more := ""
	if len(named) > maxNamedDependents {
		more = fmt.Sprintf(" and %d more", len(named)-maxNamedDependents)
		named = named[:maxNamedDependents]
	}
	return fmt.Sprintf("still has %s%s: delete or move them first", strings.Join(named, ", "), more)
}
