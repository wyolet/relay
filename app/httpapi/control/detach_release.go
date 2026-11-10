package control

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/share"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/meta"
)

// release makes the referencing row it let go of t, mirroring the detachable
// fields the references scan reports. It returns nil, leaving the row as it
// was, when the caller may not update the row or the row would not be valid
// without t; an error aborts the whole detach.
func (d Deps) release(ctx context.Context, it referenceItem, t detachTarget) (*releasedRow, error) {
	s := d.Stores
	switch it.Kind {
	case "pricing":
		return rowEdit[pricing.Pricing]{
			plural: "pricings", store: s.Pricing, validate: (*pricing.Pricing).Validate,
			metaOf: func(p *pricing.Pricing) *meta.Metadata { return &p.Meta },
			drop:   func(p *pricing.Pricing) bool { return dropKeepingOne(&p.Spec.TargetModelIDs, is(t.id)) },
		}.apply(ctx, d, it)
	case "host-binding":
		return rowEdit[binding.Binding]{
			plural: "host-bindings", store: s.Binding, validate: (*binding.Binding).Validate,
			metaOf: func(b *binding.Binding) *meta.Metadata { return &b.Meta },
			drop:   func(b *binding.Binding) bool { return clearID(&b.Spec.PricingID, t.id) },
		}.apply(ctx, d, it)
	case "role-binding":
		return rowEdit[rolebinding.RoleBinding]{
			plural: "role-bindings", store: s.RoleBinding, guard: guardRoleBinding(d),
			validate: func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
			metaOf:   func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta },
			drop:     func(b *rolebinding.RoleBinding) bool { return dropKeepingOne(&b.Spec.Subjects, t.isSubject) },
		}.apply(ctx, d, it)
	case "policy-binding":
		held, err := d.accountsOf(ctx, t)
		if err != nil {
			return nil, err
		}
		return rowEdit[policybinding.PolicyBinding]{
			plural: "policy-bindings", store: s.PolicyBinding, validate: (*policybinding.PolicyBinding).Validate, guard: guardPolicyBinding(d),
			metaOf: func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta },
			drop: func(b *policybinding.PolicyBinding) bool {
				return dropKeepingOne(&b.Spec.AccountIDs, func(id string) bool { return slices.Contains(held, id) })
			},
		}.apply(ctx, d, it)
	case "user":
		return d.releaseRole(ctx, it, t)
	}
	return nil, nil
}

// releaseShare revokes an account shared from the target, as its own delete does: by a caller who may share the real account or delete in the receiver's scope, always going through. It returns the share and every row the revoke changed.
func (d Deps) releaseShare(ctx context.Context, it referenceItem) ([]releasedRow, error) {
	a, err := d.Stores.Account.Get(ctx, it.ID)
	if err != nil || a == nil || !a.IsShared() {
		return nil, err
	}
	src, err := d.Stores.Account.Get(ctx, a.Spec.SourceAccountID)
	if err != nil {
		return nil, err
	}
	if src == nil || refs(d).MayShare(ctx, src) != nil {
		if err := d.Authz.Authorize(ctx, "accounts.delete", authz.Resource{Kind: "account", ID: a.Meta.ID, Owner: &a.Meta.Owner}); err != nil {
			return nil, failOrKeep(mapAuthzErr(err))
		}
	}
	changes, err := share.Revoke(ctx, d.shareStores(), a.Meta.ID)
	if err != nil {
		return nil, err
	}
	out := []releasedRow{{item: it, action: "accounts.delete", fields: []string{audit.AnyField}}}
	for _, c := range revokeItems(changes) {
		action := authz.Plural(c.Kind) + ".update"
		if c.deleted {
			action = authz.Plural(c.Kind) + ".delete"
		}
		out = append(out, releasedRow{item: c, action: action, fields: []string{c.Via}})
	}
	return out, nil
}

// rowEdit lets one referencing row of type T go of the target through the
// checks the row's PUT runs: visibility, the update grant, governance,
// Validate, and the kind's reference guard.
type rowEdit[T any] struct {
	plural   string
	store    entityStore[T]
	metaOf   func(*T) *meta.Metadata
	validate func(*T) error
	guard    mutationGuard[T]
	// drop removes the target from the row, false when the row cannot let go.
	drop func(*T) bool
}

// editAllowed reports whether governance permits an edit of kind under ownerKind.
func editAllowed(snap settings.Reader, kind string, ownerKind meta.OwnerKind, isAdmin bool) bool {
	return settings.Governs(snap, settings.OpEdit, kind, string(ownerKind), isAdmin) == nil
}

// valid reports whether v passes the row's validate check.
func (e rowEdit[T]) valid(v *T) bool {
	return e.validate(v) == nil
}

func (e rowEdit[T]) apply(ctx context.Context, d Deps, it referenceItem) (*releasedRow, error) {
	existing, err := e.store.Get(ctx, it.ID)
	if err != nil || existing == nil {
		return nil, err
	}
	m := e.metaOf(existing)
	if !visibleTo(ctx, d.Authz, it.Kind, it.ID, m.Owner) {
		return nil, nil
	}
	if err := d.Authz.Authorize(ctx, e.plural+".update", authz.Resource{Kind: it.Kind, ID: it.ID, Owner: &m.Owner}); err != nil {
		return nil, failOrKeep(mapAuthzErr(err))
	}
	if !editAllowed(d.Catalog, it.Kind, m.GovernedOwnerKind(), authz.IsAdmin(ctx)) {
		return nil, nil // left in place: reported as a remaining blocker
	}
	// A second copy to edit, so existing stays the before-image for the audit diff.
	next, err := e.store.Get(ctx, it.ID)
	if err != nil || next == nil {
		return nil, err
	}
	if !e.drop(next) || !e.valid(next) {
		return nil, nil // left in place: reported as a remaining blocker
	}
	if e.guard != nil {
		if err := e.guard(ctx, "update", existing, next); err != nil {
			return nil, failOrKeep(mapGuardErr(err))
		}
	}
	e.metaOf(next).Dirty = true
	if err := e.store.Upsert(ctx, next); err != nil {
		return nil, err
	}
	return &releasedRow{item: it, action: e.plural + ".update", fields: audit.DiffFields(existing, next)}, nil
}

// releaseRole drops the role from an account through the account's own
// update, so its admin-only and last-admin guards decide. Sessions are ended
// by the caller once the transaction commits.
func (d Deps) releaseRole(ctx context.Context, it referenceItem, t detachTarget) (*releasedRow, error) {
	u, err := d.Users.Get(ctx, it.ID)
	if err != nil || u == nil {
		return nil, err
	}
	roles := u.Roles
	if !dropEntries(&roles, is(t.name)) {
		return nil, nil
	}
	in := &userUpdateInput{ID: u.ID}
	in.Body.Roles = &roles
	// No revoker: ending a session cannot roll back with the transaction.
	inTx := d
	inTx.Sessions = nil
	users := directoryUsers{Store: d.Users, ops: newDirectoryOps(inTx)}
	if _, err := updateUser(ctx, users, d.Authz, in); err != nil {
		return nil, failOrKeep(err)
	}
	return &releasedRow{item: it, action: "users.update", fields: []string{"roles"}}, nil
}

// failOrKeep returns nil when err only refuses the one row — the caller may
// not edit it, or the edit would break a rule — so detach leaves the row and
// goes on. A server-side failure comes back to abort the transaction.
func failOrKeep(err error) error {
	var se huma.StatusError
	if errors.As(err, &se) && se.GetStatus() < http.StatusInternalServerError {
		return nil
	}
	return err
}

// isSubject reports whether a role binding subject names t: a service
// account by id, a group by name.
func (t detachTarget) isSubject(s rolebinding.Subject) bool {
	want := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: t.id}
	if t.kind == "group" {
		want = rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: t.name}
	}
	return s.Key() == want.Key()
}

// accountsOf is the accounts a detach target stands for in policy bindings: an account itself, or a service account's own account.
func (d Deps) accountsOf(ctx context.Context, t detachTarget) ([]string, error) {
	switch t.kind {
	case "account":
		return []string{t.id}, nil
	case "service-account":
		accounts, err := d.Stores.Account.List(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		var out []string
		for _, a := range accounts {
			if h := a.Spec.Holder; h != nil && h.Kind == account.HolderServiceAccount && h.ID == t.id {
				out = append(out, a.Meta.ID)
			}
		}
		return out, nil
	}
	return nil, nil
}

// clearID empties ref when it names id.
func clearID(ref *string, id string) bool {
	if *ref != id {
		return false
	}
	*ref = ""
	return true
}

func is(id string) func(string) bool {
	return func(s string) bool { return s == id }
}

// dropEntries removes the entries match selects, reporting whether there
// were any. A list left empty becomes nil.
func dropEntries[E any](list *[]E, match func(E) bool) bool {
	left := slices.DeleteFunc(slices.Clone(*list), match)
	if len(left) == len(*list) {
		return false
	}
	if len(left) == 0 {
		left = nil
	}
	*list = left
	return true
}

// dropKeepingOne is dropEntries for a list that needs an entry: the row lets
// go only while the list holds another.
func dropKeepingOne[E any](list *[]E, match func(E) bool) bool {
	if !slices.ContainsFunc(*list, func(e E) bool { return !match(e) }) {
		return false
	}
	return dropEntries(list, match)
}
