// The policy step the access and limits kinds share: changing the policy over a team's, project's or service account's account, in one place. The caller's reads decide the case; the step returns the rows that carry the change out, each the write its own route makes.

package pseudo

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Target names the team, project or service account whose account a kind changes.
type Target struct {
	Kind string `json:"kind" enum:"team,project,service-account" doc:"What the target is: a team or a project, whose own account is changed, or a service account, whose account (its keys' default lane) is."`
	ID   string `json:"id"                                        doc:"Id (metadata.id) of the team, project or service account; a slug answers 400."`
}

// CheckTarget refuses a target of no kind the step takes, or named by anything but its id; it needs no read.
func CheckTarget(t Target) error {
	switch t.Kind {
	case "team", "project", "service-account":
	default:
		return &BodyError{Pointer: "/spec/target/kind", Message: "spec.target.kind must be team, project or service-account"}
	}
	if !ids.Valid(t.ID) {
		return &BodyError{Pointer: "/spec/target/id", Message: fmt.Sprintf("%s %q is not an id; name the %s by its metadata.id", t.Kind, t.ID, t.Kind)}
	}
	return nil
}

// PolicyCase names what the policy step does.
type PolicyCase string

const (
	// CaseNone: the account is in no binding; a policy is made for it and bound to it alone.
	CaseNone PolicyCase = "none"
	// CaseOwn: the account is bound alone to a policy of its own scope the caller may update; that policy is changed in place.
	CaseOwn PolicyCase = "own"
	// CaseCopy: the account's policy is bound to other accounts too, is a preset, or is owned elsewhere; it is copied into the account's scope, the copy changed and bound to the account alone.
	CaseCopy PolicyCase = "copy"
	// CaseAbove: a binding or policy the caller may not see or change holds the account; refused.
	CaseAbove PolicyCase = "above"
)

// Scope names an owner scope as the caller is shown it: its kind, and its name only when the caller may see it.
type Scope struct {
	Kind string `json:"kind"           doc:"Kind of the scope: organization, system, team, project, user or host."`
	Name string `json:"name,omitempty" doc:"Name of the scope; empty when the caller may not see it, or it has none."`
}

func (s Scope) String() string {
	switch {
	case s.Kind == string(meta.OwnerOrganization) || s.Kind == string(meta.OwnerSystem):
		return "the " + s.Kind
	case s.Name != "":
		return fmt.Sprintf("%s %q", s.Kind, s.Name)
	}
	return "a " + s.Kind + " you may not see"
}

// PolicyReads is what the policy step reads of the target's account, in the caller's view. The reads stop at the first row the caller may not see or change, which sets Above.
type PolicyReads struct {
	// Account is the target's account; AccountNew when it is not made yet and is made first.
	Account    *account.Account
	AccountNew bool
	// TargetLabel is the target's display name, or its name.
	TargetLabel string
	// Above is the scope owning the binding or policy holding Account that the caller may not see.
	Above *Scope
	// Held is the binding holding Account, nil for none; Policy is its policy, nil when it names none that exists.
	Held   *policybinding.PolicyBinding
	Policy *policy.Policy
	// HeldAfter is Held without Account, as PUT /accounts/{ref}/policy leaves it when it binds Account elsewhere; nil when Held held only Account and is deleted.
	HeldAfter *policybinding.PolicyBinding
	// HeldChangeable reports whether the caller may take Account out of Held: HeldAfter's update, or Held's delete, passes; HeldScope is Held's owner.
	HeldChangeable bool
	HeldScope      Scope
	// Alone reports whether Held holds only Account and no other binding uses Policy.
	Alone bool
	// MayUpdate reports whether the caller may update Policy in place.
	MayUpdate bool
	// Uncarried counts, by kind (pool, rate-limit, budget), the links of a policy its owner may not link.
	Uncarried func(p *policy.Policy) map[string]int
	Names     *Namer
	NewID     func() string
}

// owner is the scope the step makes its rows in: the account's own.
func (in PolicyReads) owner() meta.Owner { return in.Account.Meta.Owner }

// Case decides what the step does for in.
func (in PolicyReads) Case() PolicyCase {
	switch {
	case in.Above != nil:
		return CaseAbove
	case in.Held == nil || in.Policy == nil:
		return CaseNone
	case in.Held.IsEnabled() && in.Alone && in.Policy.Meta.Owner == in.owner() && in.MayUpdate:
		return CaseOwn
	case !in.HeldChangeable:
		return CaseAbove
	}
	return CaseCopy
}

// AboveError refuses a change of an account a binding or policy above the caller holds; it names only the scope.
type AboveError struct{ Scope Scope }

func (e *AboveError) Error() string {
	return fmt.Sprintf("this account's policy is set by %s, which you may not change; ask an admin there to change it", e.Scope)
}

// UncarriedError refuses a copy that would carry links its owner may not link; Counts are by kind.
type UncarriedError struct{ Counts map[string]int }

func (e *UncarriedError) Error() string {
	kinds := make([]string, 0, len(e.Counts))
	for k := range e.Counts {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		word := strings.ReplaceAll(k, "-", " ")
		if e.Counts[k] != 1 {
			word += "s"
		}
		parts[i] = fmt.Sprintf("%d %s", e.Counts[k], word)
	}
	return "this account's policy is shared or owned elsewhere, so it is copied into the account's scope, but it links " + strings.Join(parts, " and ") + " that scope may not link; the copy cannot carry them. Remove them in the request, or ask an admin of the policy's scope"
}

// PolicyChange is what the step does: the case, the steps in write order, and the policy the account is under once they are written.
type PolicyChange struct {
	Case   PolicyCase
	Rows   []Row
	Policy *policy.Policy
}

// ChangePolicy returns the steps that put change over the policy of in.Account: none makes the account first when it is new, then a policy of change applied to an empty spec, owned by the account's scope, and binds it alone; own updates the policy in place, or writes nothing when change leaves it as it is; copy makes a copy owned by the account's scope (models, links, description and labels), applies change, refuses with *UncarriedError links the copy would still carry that the scope may not link, then takes the account out of its binding and binds it to the copy alone, as PUT /accounts/{ref}/policy does. above is an *AboveError. pointer is the part of the body the steps come from.
func ChangePolicy(in PolicyReads, change func(*policy.Spec), pointer string) (PolicyChange, error) {
	c := PolicyChange{Case: in.Case()}
	add := func(kind string, op Op, m *meta.Metadata, v any) {
		c.Rows = append(c.Rows, Row{Kind: kind, Pointer: pointer, Meta: m, Value: v, Op: op})
	}
	switch c.Case {
	case CaseAbove:
		scope := in.HeldScope
		if in.Above != nil {
			scope = *in.Above
		}
		return c, &AboveError{Scope: scope}
	case CaseOwn:
		next := *in.Policy
		next.Spec = cloneSpec(in.Policy.Spec)
		change(&next.Spec)
		c.Policy = &next
		if !reflect.DeepEqual(next.Spec, in.Policy.Spec) {
			add("policy", OpUpdate, &next.Meta, &next)
		}
		return c, nil
	}
	if in.AccountNew {
		add("account", OpCreate, &in.Account.Meta, in.Account)
	}
	p := &policy.Policy{Meta: meta.Metadata{ID: in.NewID(), Owner: in.owner()}}
	p.Meta.DisplayName = in.TargetLabel
	if c.Case == CaseCopy {
		p.Meta.DisplayName = label(in.Policy.Meta) + " for " + in.TargetLabel
		p.Meta.Description = in.Policy.Meta.Description
		p.Meta.Labels = maps.Clone(in.Policy.Meta.Labels)
		p.Spec = cloneSpec(in.Policy.Spec)
	}
	p.Meta.Name = in.Names.Mint("policy", p.Meta.DisplayName, "policy")
	change(&p.Spec)
	if c.Case == CaseCopy {
		if counts := in.Uncarried(carried(in.Policy, p)); len(counts) > 0 {
			return c, &UncarriedError{Counts: counts}
		}
	}
	add("policy", OpCreate, &p.Meta, p)
	if in.Held != nil {
		if in.HeldAfter == nil {
			add("policy-binding", OpDelete, &in.Held.Meta, in.Held)
		} else {
			add("policy-binding", OpUpdate, &in.HeldAfter.Meta, in.HeldAfter)
		}
	}
	b := bindingAlone(in.Names, in.NewID, p.Meta.ID, p.Meta.Name, in.Account)
	add("policy-binding", OpCreate, &b.Meta, b)
	c.Policy = p
	return c, nil
}

// carried is cp with only the links it carries over from src: the pools, rate limits and budgets the change left, not those it added.
func carried(src, cp *policy.Policy) *policy.Policy {
	out := *cp
	out.Spec = cloneSpec(cp.Spec)
	out.Spec.PoolIDs = slices.DeleteFunc(out.Spec.PoolIDs, func(id string) bool { return !slices.Contains(src.Spec.PoolIDs, id) })
	out.Spec.RateLimits = slices.DeleteFunc(out.Spec.RateLimits, func(l policy.RateLimitLink) bool {
		return !slices.ContainsFunc(src.Spec.RateLimits, func(s policy.RateLimitLink) bool { return s.RateLimitID == l.RateLimitID })
	})
	out.Spec.Budgets = slices.DeleteFunc(out.Spec.Budgets, func(l policy.BudgetLink) bool { return !slices.Contains(src.BudgetIDs(), l.BudgetID) })
	return &out
}

// cloneSpec is s with every list its own, so a change to the copy leaves s as it is.
func cloneSpec(s policy.Spec) policy.Spec {
	out := s
	out.Models = slices.Clone(s.Models)
	out.PoolIDs = slices.Clone(s.PoolIDs)
	out.Budgets = slices.Clone(s.Budgets)
	out.RateLimits = slices.Clone(s.RateLimits)
	for i, l := range out.RateLimits {
		out.RateLimits[i].Models = slices.Clone(l.Models)
	}
	return out
}
