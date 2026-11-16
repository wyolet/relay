// An edit to a policy reaches every account bound to it, and an edit to a rate limit or budget reaches every account bound to a policy linking it. Such an edit goes through only when the caller may set the policy of each of those accounts: every enabled binding of the policy passes the checks PUT /accounts/{ref}/policy runs on the binding holding an account. Otherwise it answers 403 policy_in_use naming the accounts, and the way on is a copy (POST /policies/{ref}/duplicate). The control API's updates and POST /apply run the same check. Out of scope: deletes, which keep their own in-use refusals, and pools.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// codePolicyInUse is the error code of an edit that reaches accounts whose policy the caller may not set.
const codePolicyInUse = "policy_in_use"

// namedInMessage caps the rows a refusal's message names; its body lists every account.
const namedInMessage = 5

// policyInUseError is the 403 a refused edit answers: the error envelope plus the accounts that block it.
type policyInUseError struct {
	httpapi.APIError
	BlockedBy policy.EditBlockers `json:"blockedBy"`
}

// policyInUseBody documents the 403 of an update the edit guard runs on.
type policyInUseBody struct {
	Err       httpapi.APIErrorBody `json:"error"               doc:"The error. code policy_in_use: the edit reaches accounts whose policy the caller may not set. Without it, a role, governance setting or scope rule refused the update."`
	BlockedBy *policy.EditBlockers `json:"blockedBy,omitempty" doc:"With policy_in_use: the accounts whose policy the caller may not set."`
}

// policyChecks are the checks the policies routes run. An update also passes the edit guard, before the model refs are normalized.
func policyChecks(d Deps) kindChecks[policy.Policy] { return policyChecksCopying(d, nil) }

// policyChecksCopying is policyChecks for POST /policies/{ref}/duplicate: copied is the links taken over from the source, which pass as the source's own did.
func policyChecksCopying(d Deps, copied map[string]bool) kindChecks[policy.Policy] {
	models := guardPolicyModels(d, copied)
	return kindChecks[policy.Policy]{
		plural: "policies", singular: "policy", authz: d.Authz, gov: d.Catalog,
		validate: func(p *policy.Policy) error { return p.Validate() },
		guard: func(ctx context.Context, action string, existing, incoming *policy.Policy) error {
			if action == "update" {
				if err := refusePolicyEdit(ctx, d, existing.Meta.ID); err != nil {
					return err
				}
			}
			return models(ctx, action, existing, incoming)
		},
	}
}

// maySetEveryPolicy reports whether the caller may set any account's policy: an admin, or anyone when nothing scopes access.
func maySetEveryPolicy(ctx context.Context, a authz.Authorizer) bool {
	return refcheck.RequireAdmin(ctx, a, "") == nil
}

// memoRefs is refs(d) with every read cached for one answer: the guard checks each binding, and the bindings share their rows.
func memoRefs(d Deps) refcheck.Checker {
	c := refs(d)
	c.Rows = c.Rows.Memoized()
	return c
}

// storedBindings lists every stored policy binding through c.
func storedBindings(ctx context.Context, c refcheck.Checker) ([]*policybinding.PolicyBinding, error) {
	if c.Rows.PolicyBindings == nil {
		return nil, nil
	}
	pbs, err := c.Rows.PolicyBindings(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("list policy-bindings: " + err.Error())
	}
	return pbs, nil
}

// policyBlockers returns the account ids held by the enabled bindings of policyID, among bindings, that the caller fails the checks on.
func policyBlockers(ctx context.Context, d Deps, c refcheck.Checker, bindings []*policybinding.PolicyBinding, policyID string) ([]string, error) {
	checks := policyBindingChecksWith(d, c)
	var ids []string
	for _, b := range bindings {
		if b.Spec.PolicyID != policyID || !b.IsEnabled() {
			continue
		}
		refused, err := bindingRefused(ctx, d, checks, b)
		if err != nil {
			return nil, err
		}
		if refused {
			ids = append(ids, b.Spec.AccountIDs...)
		}
	}
	return ids, nil
}

// bindingRefused runs on b what PUT /accounts/{ref}/policy runs on the binding holding an account: b must be visible, and its update grant, governance and reference checks must pass. A server failure is the error. d's authorizer should not be audited: these grants are probes, and the request's row must name the write it acted on.
func bindingRefused(ctx context.Context, d Deps, checks kindChecks[policybinding.PolicyBinding], b *policybinding.PolicyBinding) (bool, error) {
	if !visibleTo(ctx, d.Authz, "policy-binding", b.Meta.ID, b.Meta.Owner) {
		return true, nil
	}
	err := checks.mayUpdate(ctx, b.Meta.ID, &b.Meta)
	if err == nil {
		err = checks.guarded(ctx, "update", b, b)
	}
	if err == nil {
		return false, nil
	}
	var se huma.StatusError
	if errors.As(err, &se) && se.GetStatus() < http.StatusInternalServerError {
		return true, nil
	}
	return false, err
}

// editBlockers names the accounts ids once each: by name and kind when the caller may see them, else counted.
func editBlockers(ctx context.Context, d Deps, c refcheck.Checker, ids []string) policy.EditBlockers {
	out := policy.EditBlockers{Accounts: []policy.AccountRef{}}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var a *account.Account
		if c.Rows.Account != nil {
			a = c.Rows.Account(ctx, id)
		}
		if a == nil || !visibleTo(ctx, d.Authz, "account", a.Meta.ID, a.Meta.Owner) {
			out.Hidden++
			continue
		}
		out.Accounts = append(out.Accounts, policy.AccountRef{ID: a.Meta.ID, Name: a.Meta.Name, DisplayName: a.Meta.DisplayName, Kind: string(a.DerivedKind())})
	}
	return out
}

// refusePolicyEdit refuses an update of policyID while it is applied to an account whose policy the caller may not set.
func refusePolicyEdit(ctx context.Context, d Deps, policyID string) error {
	if d.Stores == nil || maySetEveryPolicy(ctx, d.Authz) {
		return nil
	}
	d.Authz = unaudited(d.Authz)
	c := memoRefs(d)
	bindings, err := storedBindings(ctx, c)
	if err != nil {
		return err
	}
	ids, err := policyBlockers(ctx, d, c, bindings, policyID)
	if err != nil || len(ids) == 0 {
		return err
	}
	by := editBlockers(ctx, d, c, ids)
	return policyInUse(by, fmt.Sprintf("you can't change this policy: it is applied to %s, whose policy you may not set; duplicate it and edit the copy", accountsText(by)))
}

// refuseLinkedEdit refuses an update of the rate limit or budget id while a policy linking it is applied to an account whose policy the caller may not set. The linking policies are the ones its delete takes the link from.
func refuseLinkedEdit(ctx context.Context, d Deps, kind, id string) error {
	if d.Stores == nil || maySetEveryPolicy(ctx, d.Authz) {
		return nil
	}
	d.Authz = unaudited(d.Authz)
	items, err := referencesOf(ctx, d, kind, id)
	if err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	c := memoRefs(d)
	bindings, err := storedBindings(ctx, c)
	if err != nil {
		return err
	}
	var ids, policies []string
	hiddenPolicies := 0
	for _, it := range items {
		if it.Kind != "policy" {
			continue
		}
		got, err := policyBlockers(ctx, d, c, bindings, it.ID)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			continue
		}
		ids = append(ids, got...)
		if visibleTo(ctx, d.Authz, "policy", it.ID, it.owner) {
			policies = append(policies, fmt.Sprintf("policy %q", policyLabel(ctx, c, it)))
		} else {
			hiddenPolicies++
		}
	}
	if len(ids) == 0 {
		return nil
	}
	by := editBlockers(ctx, d, c, ids)
	word := strings.ReplaceAll(kind, "-", " ")
	return policyInUse(by, fmt.Sprintf("you can't change this %s: it is linked from %s, applied to %s, whose policy you may not set; duplicate the policy and link a new %s to the copy",
		word, listText(policies, hiddenPolicies, "a policy you cannot see", "policies you cannot see"), accountsText(by), word))
}

// refuseAppliedEdits refuses a bundle updating a policy, rate limit or budget the caller's control API update of that row would be refused for, naming the document.
func refuseAppliedEdits(ctx context.Context, d Deps, plan *apply.Result) error {
	for _, e := range plan.Entries {
		if e.Action != apply.ActionUpdate {
			continue
		}
		var err error
		switch e.Kind {
		case "Policy":
			err = refusePolicyEdit(ctx, d, e.ID)
		case "RateLimit":
			err = refuseLinkedEdit(ctx, d, "rate-limit", e.ID)
		case "Budget":
			err = refuseLinkedEdit(ctx, d, "budget", e.ID)
		}
		if pe, ok := errors.AsType[*policyInUseError](err); ok {
			pe.Err.Message = fmt.Sprintf("apply: %s %q: %s", e.Kind, e.Name, pe.Err.Message)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func policyInUse(by policy.EditBlockers, msg string) error {
	return &policyInUseError{
		APIError: httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codePolicyInUse, Message: msg},
			HTTPStatus: http.StatusForbidden,
		},
		BlockedBy: by,
	}
}

// policyLabel is the linking policy's display name, or its slug.
func policyLabel(ctx context.Context, c refcheck.Checker, it referenceItem) string {
	if c.Rows.Policy != nil {
		if p := c.Rows.Policy(ctx, it.ID); p != nil {
			return policyDisplay(p.Meta)
		}
	}
	return it.Name
}

// accountsText names blocking accounts for a message, such as `team "Research"`.
func accountsText(by policy.EditBlockers) string {
	names := make([]string, 0, len(by.Accounts))
	for _, a := range by.Accounts {
		names = append(names, accountText(a))
	}
	return listText(names, by.Hidden, "an account you cannot see", "accounts you cannot see")
}

func accountText(a policy.AccountRef) string {
	label := a.DisplayName
	if label == "" {
		label = a.Name
	}
	switch account.Kind(a.Kind) {
	case account.KindTeam, account.KindProject:
		return fmt.Sprintf("%s %q", a.Kind, label)
	case "":
		return fmt.Sprintf("account %q", label)
	}
	return fmt.Sprintf("%s account %q", a.Kind, label)
}

// listText joins names for a message: at most namedInMessage of them, then how many more, then the hidden rows counted.
func listText(names []string, hidden int, oneHidden, manyHidden string) string {
	parts := slices.Clone(names)
	if len(parts) > namedInMessage {
		parts = append(parts[:namedInMessage], fmt.Sprintf("%d more", len(names)-namedInMessage))
	}
	switch {
	case hidden == 1:
		parts = append(parts, oneHidden)
	case hidden > 1:
		parts = append(parts, fmt.Sprintf("%d %s", hidden, manyHidden))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
