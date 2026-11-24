// The access pseudo kind: what an existing team, project or service account may use, stated whole, put on the policy over its account by the policy step.

package pseudo

import (
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// AccessLabel is the label carrying the target account's id on the pools the access kind makes.
const AccessLabel = "access-for"

// Access is the body of POST /pseudo-access.
type Access struct {
	Spec AccessSpec `json:"spec" doc:"The target and its whole access."`
}

// AccessSpec names the target and what it may use.
type AccessSpec struct {
	Target Target       `json:"target" doc:"The team, project or service account whose account's access is set."`
	Access *AgentAccess `json:"access,omitempty" doc:"The whole access, with the meaning and rules of POST /pseudo-agents' access: pools and models the policy links now and access no longer names are unlinked; its rate limits and budgets are kept. Omitted, with dryRun=true, answers only case and current; otherwise required."`
}

// AccessReads is what expanding an access body reads besides it.
type AccessReads struct {
	Policy PolicyReads
	// Accounts are the accounts access.accountIds names, by id; HostNames and KeylessHosts are of their hosts.
	Accounts     map[string]*account.Account
	HostNames    map[string]string
	KeylessHosts map[string]bool
	// Reaches reports whether a pool of the target's scope may list the real account a without a share; Shares are that scope's shares, by the real account's id.
	Reaches func(a *account.Account) bool
	Shares  map[string]*account.Account
	// OwnerName is the name of the target's scope; a share's name carries it.
	OwnerName string
	// Labelled are the pools labelled AccessLabel=<target account id> the caller may see; LinkedBy are the ids of the policies linking each, by pool id.
	Labelled []*pool.Pool
	LinkedBy map[string][]string
	// HiddenPools are the pools the policy links that the caller may not see, by id.
	HiddenPools map[string]bool
}

// AccessChange is what an access body expands to.
type AccessChange struct {
	PolicyChange
	// Unchanged is set when nothing needed writing: an account in no binding already uses every credential and every model.
	Unchanged bool
	// HiddenPoolsKept counts the pool links the caller may not see that the policy keeps.
	HiddenPoolsKept int
}

// ExpandAccess returns the steps that set the target's access: the shares access.accountIds needs, one pool per host (the labelled pool for that host updated in place when only the policy changed in place links it, else a new one labelled AccessLabel), the policy step setting the policy's pools to poolIds then those pools (none with everyCredential) and its models to access.models, then the delete of each labelled pool the policy no longer links that nothing else links. A pool link the caller may not see is kept where it was in the policy's order, in place and in a copy, as an update of the policy keeps the links it holds.
func ExpandAccess(body Access, in AccessReads) (AccessChange, error) {
	if err := CheckAccess(body); err != nil {
		return AccessChange{}, err
	}
	a := body.Spec.Access
	pc := in.Policy.Case()
	if pc == CaseAbove {
		c, err := ChangePolicy(in.Policy, func(*policy.Spec) {}, "/spec/target")
		return AccessChange{PolicyChange: c}, err
	}
	if pc == CaseNone && a.EveryCredential && len(a.Models) == 0 {
		return AccessChange{PolicyChange: PolicyChange{Case: pc}, Unchanged: true}, nil
	}
	x := accessExpansion{in: in, reused: map[string]bool{}}
	for _, id := range a.PoolIDs {
		x.reused[id] = true
	}
	if pc == CaseOwn {
		x.inPlace = in.Policy.Policy.Meta.ID
	}
	var poolIDs []string
	if !a.EveryCredential {
		made, err := x.pools(a.AccountIDs)
		if err != nil {
			return AccessChange{}, err
		}
		poolIDs = append(slices.Clone(a.PoolIDs), made...)
	}
	kept := 0
	if p := in.Policy.Policy; p != nil {
		poolIDs, kept = keepHidden(p.Spec.PoolIDs, poolIDs, in.HiddenPools)
	}
	models := slices.Clone(a.Models)
	c, err := ChangePolicy(in.Policy, func(s *policy.Spec) { s.PoolIDs, s.Models = poolIDs, models }, "/spec/access")
	if err != nil {
		return AccessChange{}, err
	}
	rows := append(x.rows, c.Rows...)
	for _, p := range in.Labelled {
		if !slices.Contains(poolIDs, p.Meta.ID) && x.onlyInPlace(p) {
			rows = append(rows, Row{Kind: "pool", Pointer: "/spec/access", Meta: &p.Meta, Value: p, Op: OpDelete})
		}
	}
	c.Rows = rows
	return AccessChange{PolicyChange: c, HiddenPoolsKept: kept}, nil
}

// keepHidden is named with each pool of current that hidden holds put back at its index in current (or last, past the end), and how many it put back.
func keepHidden(current, named []string, hidden map[string]bool) ([]string, int) {
	out := slices.Clone(named)
	kept := 0
	for i, id := range current {
		if !hidden[id] || slices.Contains(out, id) {
			continue
		}
		out = slices.Insert(out, min(i, len(out)), id)
		kept++
	}
	return out, kept
}

// CheckAccess refuses a body whose target or access is malformed, access left out included, or names a row by anything but its id; it needs no read, so a caller runs it before reading what the body names. ExpandAccess runs it too.
func CheckAccess(body Access) error {
	if err := CheckTarget(body.Spec.Target); err != nil {
		return err
	}
	if body.Spec.Access == nil {
		return &BodyError{Pointer: "/spec/access", Message: "spec.access is required unless dryRun=true"}
	}
	return checkAccess(body.Spec.Access)
}

type accessExpansion struct {
	in AccessReads
	// inPlace is the id of the policy the step updates in place, "" when it makes one.
	inPlace string
	rows    []Row
	// reused are the pools poolIds lists or a host took already.
	reused map[string]bool
}

func (x *accessExpansion) add(kind, pointer string, m *meta.Metadata, v any) {
	x.rows = append(x.rows, Row{Kind: kind, Pointer: pointer, Meta: m, Value: v})
}

// onlyInPlace reports whether no policy but the one updated in place links p.
func (x *accessExpansion) onlyInPlace(p *pool.Pool) bool {
	for _, id := range x.in.LinkedBy[p.Meta.ID] {
		if id != x.inPlace {
			return false
		}
	}
	return true
}

// pools returns the ids of one pool per host of accountIDs, in the order the hosts first appear: a labelled pool at the host that only the policy updated in place links, its accounts replaced, or a new labelled pool.
func (x *accessExpansion) pools(accountIDs []string) ([]string, error) {
	in, acc := x.in, x.in.Policy.Account
	src := accountSources{accounts: in.Accounts, hostNames: in.HostNames, keyless: in.KeylessHosts, reaches: in.Reaches, shares: in.Shares, ownerName: in.OwnerName, names: in.Policy.Names, newID: in.Policy.NewID}
	hosts, members, err := poolMembers(src, acc.Meta.Owner, accountIDs, x.add)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if old := x.reusable(h); old != nil {
			ids = append(ids, old.Meta.ID)
			if !slices.Equal(old.Spec.AccountIDs, members[h]) {
				next := *old
				next.Spec.AccountIDs = members[h]
				x.rows = append(x.rows, Row{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &next.Meta, Value: &next, Op: OpUpdate})
			}
			continue
		}
		display := fmt.Sprintf("%s at %s", in.Policy.TargetLabel, in.HostNames[h])
		p := &pool.Pool{
			Meta: meta.Metadata{ID: in.Policy.NewID(), Name: in.Policy.Names.Mint("pool", display, "pool"), DisplayName: display, Owner: acc.Meta.Owner, Labels: map[string]string{AccessLabel: acc.Meta.ID}},
			Spec: pool.Spec{HostID: h, AccountIDs: members[h]},
		}
		x.add("pool", "/spec/access/accountIds", &p.Meta, p)
		ids = append(ids, p.Meta.ID)
	}
	return ids, nil
}

// reusable is a labelled pool at host h, neither listed in poolIds nor taken already, that only the policy updated in place links; nil for none.
func (x *accessExpansion) reusable(h string) *pool.Pool {
	for _, p := range x.in.Labelled {
		if p.Spec.HostID == h && !x.reused[p.Meta.ID] && x.onlyInPlace(p) {
			x.reused[p.Meta.ID] = true
			return p
		}
	}
	return nil
}
