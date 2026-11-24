// What the policy step of the access and limits kinds reads, in the caller's view: the target's account, made first when relay has not made it yet, the binding holding it and that binding's policy. The reads stop at the first of those the caller may not see, before anything else of it is read. The refusals the step answers are here too.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// codePolicySetAbove refuses a change of an account a binding or policy the caller may not see or change holds.
const codePolicySetAbove = "policy_set_above"

// codeLinksNotCarried refuses a copy that would carry links the target's scope may not link.
const codeLinksNotCarried = "links_not_carried"

// codeHiddenPoolsKept refuses everyCredential on a policy whose pool links the caller may not see would stay and narrow it to them.
const codeHiddenPoolsKept = "hidden_pools_kept"

// policyReads reads what the policy step needs for target t.
func (d Deps) policyReads(ctx context.Context, t pseudo.Target) (pseudo.PolicyReads, error) {
	in := pseudo.PolicyReads{Names: d.pseudoNamer(), NewID: ids.New}
	want, label, err := d.targetAccount(ctx, t)
	if err != nil {
		return in, err
	}
	in.TargetLabel = label
	if in.Account, err = d.managedAccount(ctx, want); err != nil {
		return in, err
	}
	if in.Account == nil {
		want.Meta.ID = in.NewID()
		want.Meta.Name = in.Names.Mint("account", want.Meta.Name, "account")
		in.Account, in.AccountNew = want, true
		return in, nil
	}
	held, err := d.Stores.PolicyBinding.Holding(ctx, in.Account.Meta.ID)
	if err != nil || held == nil {
		return in, err
	}
	pseudo.Record(ctx, held.Meta.ID, held)
	if !visibleTo(ctx, d.Authz, "policy-binding", held.Meta.ID, held.Meta.Owner) {
		scope := d.scopeOf(ctx, held.Meta.Owner)
		in.Above = &scope
		return in, nil
	}
	in.Held, in.HeldScope = held, d.scopeOf(ctx, held.Meta.Owner)
	moved := ""
	c := planPolicyChange(held, in.Account.Meta.ID, &moved)
	if in.HeldChangeable, err = d.mayTakeOut(ctx, held, c); err != nil {
		return in, err
	}
	p, err := d.Stores.Policy.Get(ctx, held.Spec.PolicyID)
	if err != nil || p == nil {
		return in, err
	}
	pseudo.Record(ctx, p.Meta.ID, p)
	if !visibleTo(ctx, d.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
		scope := d.scopeOf(ctx, p.Meta.Owner)
		in.Above = &scope
		return in, nil
	}
	in.Policy = p
	if in.Alone, err = d.boundAlone(ctx, held, in.Account.Meta.ID); err != nil {
		return in, err
	}
	if in.Alone && p.Meta.Owner == in.Account.Meta.Owner {
		if in.MayUpdate, err = d.mayUpdatePolicy(ctx, p); err != nil {
			return in, err
		}
	}
	in.Uncarried = func(p *policy.Policy) map[string]int { return d.uncarriedLinks(ctx, p) }
	return in, nil
}

// targetAccount is the caller account target t stands for, as relay keeps it, and the target's label; a target the caller may not see answers 404.
func (d Deps) targetAccount(ctx context.Context, t pseudo.Target) (*account.Account, string, error) {
	found := func(err error, ok bool) error {
		if err != nil {
			return err
		}
		if !ok {
			return refusal(huma.Error404NotFound(fmt.Sprintf("%s %q not found", t.Kind, t.ID)), "/spec/target/id")
		}
		return nil
	}
	switch t.Kind {
	case "team":
		tm, err := d.Stores.Team.Get(ctx, t.ID)
		if err := found(err, tm != nil && visibleTo(ctx, d.Authz, "team", tm.Meta.ID, tm.Meta.Owner)); err != nil {
			return nil, "", err
		}
		pseudo.Record(ctx, tm.Meta.ID, tm)
		return account.ForTeam(tm.Meta.ID, tm.Meta.Name, tm.Meta.DisplayName), policyDisplay(tm.Meta), nil
	case "project":
		p, err := d.Stores.Project.Get(ctx, t.ID)
		if err := found(err, p != nil && visibleTo(ctx, d.Authz, "project", p.Meta.ID, p.Meta.Owner)); err != nil {
			return nil, "", err
		}
		pseudo.Record(ctx, p.Meta.ID, p)
		return account.ForProject(p.Meta.ID, p.Meta.Name, p.Meta.DisplayName), policyDisplay(p.Meta), nil
	case "service-account":
		sa, err := d.Stores.ServiceAccount.Get(ctx, t.ID)
		if err := found(err, sa != nil && visibleTo(ctx, d.Authz, "service-account", sa.Meta.ID, sa.Meta.Owner)); err != nil {
			return nil, "", err
		}
		pseudo.Record(ctx, sa.Meta.ID, sa)
		return account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Spec.ProjectID), policyDisplay(sa.Meta), nil
	}
	return nil, "", refusal(huma.Error400BadRequest("spec.target.kind must be team, project or service-account"), "/spec/target/kind")
}

// managedAccount is the stored caller account want describes, nil when relay has not made it yet.
func (d Deps) managedAccount(ctx context.Context, want *account.Account) (*account.Account, error) {
	all, err := d.Stores.Account.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.ManagedKey() == want.ManagedKey() {
			pseudo.Record(ctx, a.Meta.ID, a)
			return a, nil
		}
	}
	return nil, nil
}

// mayTakeOut reports whether the caller may take the account out of held as c decides: held's delete, or its update to c.shrunk, passes its checks. The probes are not audited.
func (d Deps) mayTakeOut(ctx context.Context, held *policybinding.PolicyBinding, c policyChange) (bool, error) {
	pd := d
	pd.Authz = unaudited(d.Authz)
	checks := policyBindingChecks(pd)
	if c.deleteHeld {
		err := checks.mayDelete(ctx, held.Meta.ID, &held.Meta)
		if err == nil {
			err = checks.guarded(ctx, "delete", held, nil)
		}
		return probeAllowed(err)
	}
	err := checks.mayUpdate(ctx, held.Meta.ID, &held.Meta)
	if err == nil {
		err = checks.guarded(ctx, "update", held, c.shrunk)
	}
	return probeAllowed(err)
}

// mayUpdatePolicy reports whether the caller may update p in place: its update grant, governance and edit guard pass. The probes are not audited.
func (d Deps) mayUpdatePolicy(ctx context.Context, p *policy.Policy) (bool, error) {
	pd := d
	pd.Authz = unaudited(d.Authz)
	err := policyChecks(pd).mayUpdate(ctx, p.Meta.ID, &p.Meta)
	if err == nil {
		err = refusePolicyEdit(ctx, pd, p.Meta.ID)
	}
	return probeAllowed(err)
}

// probeAllowed reads a check's answer as a yes or no; a server failure is the error.
func probeAllowed(err error) (bool, error) {
	var se huma.StatusError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &se) && se.GetStatus() < http.StatusInternalServerError:
		return false, nil
	}
	return false, err
}

// boundAlone reports whether held holds only accountID and no other binding uses its policy.
func (d Deps) boundAlone(ctx context.Context, held *policybinding.PolicyBinding, accountID string) (bool, error) {
	if !slices.Equal(held.Spec.AccountIDs, []string{accountID}) {
		return false, nil
	}
	all, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return false, err
	}
	for _, b := range all {
		if b.Meta.ID != held.Meta.ID && b.Spec.PolicyID == held.Spec.PolicyID {
			return false, nil
		}
	}
	return true, nil
}

// uncarriedLinks counts, by kind, the links p carries over from the policy it copies that a policy of p's owner may not link, as the copy's create (POST /policies/{ref}/duplicate's checks) would refuse them: a link the caller may not see is held, as the source's own was, and only the owner rules apply.
func (d Deps) uncarriedLinks(ctx context.Context, p *policy.Policy) map[string]int {
	c := refs(d)
	c.Held = refcheck.PolicyLinks(p)
	out := map[string]int{}
	for _, id := range p.Spec.PoolIDs {
		if c.PoolRefs(ctx, []string{id}, p.Meta.Owner) != nil {
			out["pool"]++
		}
	}
	for _, l := range p.Spec.RateLimits {
		if c.RateLimitRef(ctx, l.RateLimitID, p.Meta.Owner) != nil {
			out["rate-limit"]++
		}
	}
	for _, id := range p.BudgetIDs() {
		if c.BudgetRefs(ctx, []string{id}, p.Meta.Owner) != nil {
			out["budget"]++
		}
	}
	return out
}

// scopeOf names owner scope o as the caller is shown it: its name only when the caller may see it.
func (d Deps) scopeOf(ctx context.Context, o meta.Owner) pseudo.Scope {
	s := pseudo.Scope{Kind: string(o.Kind)}
	switch o.Kind {
	case meta.OwnerTeam:
		if t, err := d.Stores.Team.Get(ctx, o.ID); err == nil && t != nil && visibleTo(ctx, d.Authz, "team", t.Meta.ID, t.Meta.Owner) {
			s.Name = t.Meta.Name
		}
	case meta.OwnerProject:
		if p, err := d.Stores.Project.Get(ctx, o.ID); err == nil && p != nil && visibleTo(ctx, d.Authz, "project", p.Meta.ID, p.Meta.Owner) {
			s.Name = p.Meta.Name
		}
	case meta.OwnerUser:
		if d.userVisible(ctx, o.ID) {
			s.Name = d.ownerName(ctx, o)
		}
	case meta.OwnerHost:
		if h, err := d.Stores.Host.Get(ctx, o.ID); err == nil && h != nil && visibleTo(ctx, d.Authz, "host", h.Meta.ID, h.Meta.Owner) {
			s.Name = h.Meta.Name
		}
	}
	return s
}

// policyStepError answers the policy step's own refusals in the pseudo refusal envelope; any other error is returned as it is.
func policyStepError(err error) error {
	var ae *pseudo.AboveError
	var ue *pseudo.UncarriedError
	var he *pseudo.HiddenPoolsError
	switch {
	case errors.As(err, &ae):
		return refusal(&httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: codePolicySetAbove, Message: ae.Error()}, HTTPStatus: http.StatusForbidden}, "/spec/target")
	case errors.As(err, &ue):
		return refusal(&httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeLinksNotCarried, Message: ue.Error()}, HTTPStatus: http.StatusBadRequest}, "/spec/target")
	case errors.As(err, &he):
		return refusal(&httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeHiddenPoolsKept, Message: he.Error()}, HTTPStatus: http.StatusConflict}, "/spec/access/everyCredential")
	}
	return err
}
