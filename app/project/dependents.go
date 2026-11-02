package project

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
)

// Rows are the rows that can live under a team or project. Deleting the
// owner would cascade through the foreign keys to its projects, service
// accounts, their keys and its policy bindings, and leave its policies, host
// keys, rate limits, accounts, pools and budgets owned by a team or project
// that no longer exists — so apply's prune refuses while any remain, as the
// control API's delete does. A default account no key names and a caller
// account are the exceptions: the delete takes them along.
type Rows struct {
	ServiceAccounts []*serviceaccount.ServiceAccount
	Keys            []*key.Key
	Policies        []*policy.Policy
	HostKeys        []*hostkey.HostKey
	RateLimits      []*ratelimit.RateLimit
	PolicyBindings  []*policybinding.PolicyBinding
	Accounts        []*account.Account
	Pools           []*pool.Pool
	Budgets         []*budget.Budget
}

// Gone reports a row the caller is deleting in the same operation, keyed by
// manifest kind and id. Such a row is not in the way.
type Gone func(kind, id string) bool

// Visible reports a row the caller may see, keyed by manifest kind. Nil sees
// every row.
type Visible func(kind string, m meta.Metadata) bool

type dependents struct {
	gone    Gone
	visible Visible
	named   []string
	hidden  int
}

func (d *dependents) add(kind string, m meta.Metadata, under bool) {
	switch {
	case !under || (d.gone != nil && d.gone(kind, m.ID)):
	case d.visible != nil && !d.visible(kind, m):
		d.hidden++
	default:
		d.named = append(d.named, kind+"/"+m.Name)
	}
}

// Dependents names, as "Kind/name", every row in rows under project id the
// caller may see, and counts the rest.
func (r Rows) Dependents(id string, gone Gone, visible Visible) (named []string, hidden int) {
	d := &dependents{gone: gone, visible: visible}
	owner := meta.Owner{Kind: meta.OwnerProject, ID: id}
	for _, x := range r.ServiceAccounts {
		d.add("ServiceAccount", x.Meta, x.Spec.ProjectID == id)
	}
	for _, x := range r.Keys {
		d.add("Key", x.Meta, x.Meta.Owner == owner)
	}
	r.addOwned(d, owner)
	for _, x := range r.PolicyBindings {
		d.add("PolicyBinding", x.Meta, x.Meta.Owner == owner)
	}
	return d.named, d.hidden
}

// TeamDependents names, as "Kind/name", every project in projects under team
// id and every row in rows the team owns that the caller may see, and counts
// the rest.
func (r Rows) TeamDependents(id string, projects []*Project, gone Gone, visible Visible) (named []string, hidden int) {
	d := &dependents{gone: gone, visible: visible}
	owner := meta.Owner{Kind: meta.OwnerTeam, ID: id}
	for _, x := range projects {
		d.add("Project", x.Meta, x.Spec.TeamID == id)
	}
	r.addOwned(d, owner)
	for _, x := range r.PolicyBindings {
		d.add("PolicyBinding", x.Meta, x.Meta.Owner == owner)
	}
	return d.named, d.hidden
}

// addOwned adds the rows whose owner lives only in metadata, which no delete
// takes along.
func (r Rows) addOwned(d *dependents, owner meta.Owner) {
	for _, x := range r.Policies {
		d.add("Policy", x.Meta, x.Meta.Owner == owner)
	}
	for _, x := range r.HostKeys {
		d.add("HostKey", x.Meta, x.Meta.Owner == owner)
	}
	for _, x := range r.RateLimits {
		d.add("RateLimit", x.Meta, x.Meta.Owner == owner)
	}
	// A default account no remaining key names goes with its owner.
	held := map[string]bool{}
	for _, k := range r.HostKeys {
		if d.gone == nil || !d.gone("HostKey", k.Meta.ID) {
			held[k.Spec.AccountID] = true
		}
	}
	// A caller account goes with its team or project too, and takes the keys running in it along, so those keys are what stands in the way.
	callers := map[string]bool{}
	for _, x := range r.Accounts {
		if x.Meta.Owner == owner && !x.IsProvider() {
			callers[x.Meta.ID] = true
			continue
		}
		d.add("Account", x.Meta, x.Meta.Owner == owner && (!x.Spec.Default || held[x.Meta.ID]))
	}
	for _, x := range r.Keys {
		d.add("Key", x.Meta, callers[x.Spec.AccountID] && x.Meta.Owner != owner)
	}
	for _, x := range r.Pools {
		d.add("Pool", x.Meta, x.Meta.Owner == owner)
	}
	for _, x := range r.Budgets {
		d.add("Budget", x.Meta, x.Meta.Owner == owner)
	}
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
