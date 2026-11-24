// The agent removal pseudo kind: a service account and what was made for it, deleted together in an order every ordinary delete accepts. The policies and pools labelled for it go only when its owner scope owns them, the caller could delete them, and nothing left links them.

package pseudo

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/share"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Removal is a delete step whose route also changes or deletes other rows: a share's revoke takes it out of bindings and pools, a service account takes its account along through a foreign key. Row is the row deleted; Along are the others, each with its kind, metadata and Op, as the write left them. The write fills Along; an expansion may fill it with what it expects, which the write replaces.
type Removal struct {
	Row   any
	Along []Row
}

// AgentRemoval is the body of POST /pseudo-agent-removals.
type AgentRemoval struct {
	Spec AgentRemovalSpec `json:"spec" doc:"The service account to remove, and what of its labelled rows to keep."`
}

// AgentRemovalSpec names the service account, the labelled rows to keep, and the set a dry run answered.
type AgentRemovalSpec struct {
	ServiceAccountID string   `json:"serviceAccountId"  doc:"Id (metadata.id) of the service account; a slug answers 400."`
	Keep             []string `json:"keep,omitempty"    doc:"Ids of policies or pools labelled agent-id=<service account id> to leave in place. An id that is not one of the labelled rows the answer lists answers 400."`
	Expect           []string `json:"expect,omitempty"  doc:"The ids of a dry run's deleted. Given, the request answers 409 changed and deletes nothing when the rows it would delete are no longer exactly these."`
}

// KeptReason says why a labelled policy or pool stays.
type KeptReason string

const (
	// KeptInUse: a binding or policy the request does not delete still uses it, seen by the caller or not.
	KeptInUse KeptReason = "in_use"
	// KeptByRequest: spec.keep names it.
	KeptByRequest KeptReason = "kept_by_request"
	// KeptNotVisible: it is owned by the service account's scope, but the caller may not see it.
	KeptNotVisible KeptReason = "not_visible"
	// KeptNotDeletable: it is owned outside the service account's scope, or its delete's grant or governance refuses the caller.
	KeptNotDeletable KeptReason = "not_deletable"
)

// Labelled is a policy or pool labelled AgentLabel with the service account's id, and what the caller may do with it.
type Labelled struct {
	// Kind is policy or pool; Value is the *policy.Policy or *pool.Pool, Meta its metadata.
	Kind  string
	Meta  *meta.Metadata
	Value any
	// Visible: the caller may see it. InScope: the service account's owner scope owns it. Deletable: its delete's grant and governance pass for the caller.
	Visible, InScope, Deletable bool
}

// RemovalReads is what expanding an agent removal reads besides its body.
type RemovalReads struct {
	ServiceAccount *serviceaccount.ServiceAccount
	// Lane is the service account's account, nil when relay has not made it; Held the binding holding it, nil for none.
	Lane *account.Account
	Held *policybinding.PolicyBinding
	// Keys are the relay keys of the service account; Shares the shares it holds.
	Keys   []*key.Key
	Shares []*account.Account
	// Bindings are every policy binding and Policies every policy, those the caller may not see included: what still uses a labelled row.
	Bindings []*policybinding.PolicyBinding
	Policies []*policy.Policy
	// Labelled are the labelled policies, then the labelled pools.
	Labelled []Labelled
}

// KeptRow is a labelled row that stays, with why.
type KeptRow struct {
	Kind    string
	Meta    *meta.Metadata
	Visible bool
	Reason  KeptReason
}

// RemovalChange is what an agent removal expands to: the delete steps in order, and the labelled rows listed as kept.
type RemovalChange struct {
	Rows []Row
	Kept []KeptRow
}

// removalPointer is where every step of a removal comes from.
const removalPointer = "/spec/serviceAccountId"

// ExpandAgentRemoval returns the steps that remove the service account, in an order each ordinary delete accepts: its keys; its account out of the binding holding it (the binding goes when it held only that account, else shrinks); each share it holds revoked; the service account, which takes its account along; then each labelled policy, then each labelled pool, that its scope owns, the caller may see and delete, keep does not name, and no binding or policy left after the earlier steps uses. Every other labelled row the caller may see, or its scope owns, is listed as kept with why; one owned elsewhere that the caller may not see is neither touched nor listed.
func ExpandAgentRemoval(body AgentRemoval, in RemovalReads) (RemovalChange, error) {
	if err := CheckAgentRemoval(body); err != nil {
		return RemovalChange{}, err
	}
	if err := checkKeep(body.Spec.Keep, in.Labelled); err != nil {
		return RemovalChange{}, err
	}
	x := removalExpansion{bindings: map[string]*policybinding.PolicyBinding{}}
	for _, b := range in.Bindings {
		x.bindings[b.Meta.ID] = b
	}
	for _, k := range in.Keys {
		x.add("key", &k.Meta, k, OpDelete)
	}
	if in.Lane != nil && in.Held != nil {
		x.unbind(in.Lane, in.Held)
	}
	for _, s := range in.Shares {
		x.revoke(s)
	}
	sa := &Removal{Row: in.ServiceAccount}
	if in.Lane != nil {
		sa.Along = []Row{{Kind: "account", Pointer: removalPointer, Meta: &in.Lane.Meta, Value: in.Lane, Op: OpDelete}}
	}
	x.add("service-account", &in.ServiceAccount.Meta, sa, OpDelete)
	var c RemovalChange
	deletedPolicies := map[string]bool{}
	for _, l := range in.Labelled {
		inUse := false
		switch l.Kind {
		case "policy":
			inUse = x.bound(l.Meta.ID)
		case "pool":
			inUse = slices.ContainsFunc(in.Policies, func(p *policy.Policy) bool {
				return !deletedPolicies[p.Meta.ID] && slices.Contains(p.Spec.PoolIDs, l.Meta.ID)
			})
		}
		reason, listed := keptReason(l, slices.Contains(body.Spec.Keep, l.Meta.ID), inUse)
		switch {
		case !listed:
		case reason != "":
			c.Kept = append(c.Kept, KeptRow{Kind: l.Kind, Meta: l.Meta, Visible: l.Visible, Reason: reason})
		default:
			if l.Kind == "policy" {
				deletedPolicies[l.Meta.ID] = true
			}
			x.add(l.Kind, l.Meta, l.Value, OpDelete)
		}
	}
	c.Rows = x.rows
	return c, nil
}

// CheckAgentRemoval refuses a body that names the service account or a kept row by anything but its id, or a kept row twice; it needs no read. ExpandAgentRemoval runs it too.
func CheckAgentRemoval(body AgentRemoval) error {
	s := body.Spec
	if !ids.Valid(s.ServiceAccountID) {
		return &BodyError{Pointer: removalPointer, Message: fmt.Sprintf("service account %q is not an id; name the service account by its metadata.id", s.ServiceAccountID)}
	}
	seen := map[string]bool{}
	for i, id := range s.Keep {
		if !ids.Valid(id) || seen[id] {
			return &BodyError{Pointer: fmt.Sprintf("/spec/keep/%d", i), Message: fmt.Sprintf("keep entry %q is not an id, or is listed twice", id)}
		}
		seen[id] = true
	}
	return nil
}

// checkKeep refuses a kept id that is no labelled row the answer would list.
func checkKeep(keep []string, labelled []Labelled) error {
	for i, id := range keep {
		if !slices.ContainsFunc(labelled, func(l Labelled) bool { return l.Meta.ID == id && (l.Visible || l.InScope) }) {
			return &BodyError{Pointer: fmt.Sprintf("/spec/keep/%d", i), Message: fmt.Sprintf("%q is not a policy or pool labelled %s for this service account", id, AgentLabel)}
		}
	}
	return nil
}

// keptReason is why l stays, "" when it goes, and whether the answer lists it at all.
func keptReason(l Labelled, keep, inUse bool) (KeptReason, bool) {
	switch {
	case !l.InScope && !l.Visible:
		return "", false
	case !l.InScope:
		return KeptNotDeletable, true
	case !l.Visible:
		return KeptNotVisible, true
	case keep:
		return KeptByRequest, true
	case inUse:
		return KeptInUse, true
	case !l.Deletable:
		return KeptNotDeletable, true
	}
	return "", true
}

// removalExpansion keeps the steps and the bindings as the steps so far leave them.
type removalExpansion struct {
	rows     []Row
	bindings map[string]*policybinding.PolicyBinding
}

func (x *removalExpansion) add(kind string, m *meta.Metadata, v any, op Op) {
	x.rows = append(x.rows, Row{Kind: kind, Pointer: removalPointer, Meta: m, Value: v, Op: op})
}

// unbind takes lane out of held, as PUT /accounts/{ref}/policy with a null policyId does: held goes when lane was its only account, else shrinks.
func (x *removalExpansion) unbind(lane *account.Account, held *policybinding.PolicyBinding) {
	rb := &AccountRebinding{Account: lane, Held: held, Left: x.without(held, lane.Meta.ID)}
	x.rows = append(x.rows, Row{Kind: "policy-binding", Pointer: removalPointer, Meta: &rb.Binding.Meta, Value: rb})
}

// revoke deletes share s with what its revoke is expected to do to the bindings holding it.
func (x *removalExpansion) revoke(s *account.Account) {
	rm := &Removal{Row: s}
	var bindings []*policybinding.PolicyBinding
	for _, b := range x.bindings {
		bindings = append(bindings, b)
	}
	slices.SortFunc(bindings, func(a, b *policybinding.PolicyBinding) int { return strings.Compare(a.Meta.ID, b.Meta.ID) })
	for _, c := range share.Impact(share.Rows{Bindings: bindings}, s.Meta.ID) {
		held := x.bindings[c.ID]
		if left := x.without(held, s.Meta.ID); left != nil {
			rm.Along = append(rm.Along, Row{Kind: c.Kind, Pointer: removalPointer, Meta: &left.Meta, Value: left, Op: OpUpdate})
		} else {
			rm.Along = append(rm.Along, Row{Kind: c.Kind, Pointer: removalPointer, Meta: &held.Meta, Value: held, Op: OpDelete})
		}
	}
	x.add("account", &s.Meta, rm, OpDelete)
}

// without is b without accountID as the bindings so far hold it, nil when that leaves it empty and it goes.
func (x *removalExpansion) without(b *policybinding.PolicyBinding, accountID string) *policybinding.PolicyBinding {
	left := slices.DeleteFunc(slices.Clone(b.Spec.AccountIDs), func(id string) bool { return id == accountID })
	if len(left) == 0 {
		delete(x.bindings, b.Meta.ID)
		return nil
	}
	next := *b
	next.Spec.AccountIDs = left
	x.bindings[b.Meta.ID] = &next
	return &next
}

// bound reports whether a binding left by the steps so far uses policy id.
func (x *removalExpansion) bound(id string) bool {
	for _, b := range x.bindings {
		if b.Spec.PolicyID == id {
			return true
		}
	}
	return false
}
