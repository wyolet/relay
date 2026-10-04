package control

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
)

// release makes the referencing row it let go of t, mirroring the detachable
// fields the references scan reports. It returns nil, leaving the row as it
// was, when the caller may not update the row or the row would not be valid
// without t; an error aborts the whole detach.
func (d Deps) release(ctx context.Context, it referenceItem, t detachTarget) (*releasedRow, error) {
	s := d.Stores
	switch it.Kind {
	case "key":
		return rowEdit[key.Key]{
			plural: "keys", store: s.Key, validate: (*key.Key).Validate, guard: guardKeyPolicy(d),
			metaOf: func(k *key.Key) *meta.Metadata { return &k.Meta },
			drop:   func(k *key.Key) bool { return clearID(&k.Spec.PolicyID, t.id) },
		}.apply(ctx, d, it)
	case "service-account":
		return rowEdit[serviceaccount.ServiceAccount]{
			plural: "service-accounts", store: s.ServiceAccount, validate: (*serviceaccount.ServiceAccount).Validate, guard: guardServiceAccount(d),
			metaOf: func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta },
			drop:   func(sa *serviceaccount.ServiceAccount) bool { return clearID(&sa.Spec.PolicyID, t.id) },
		}.apply(ctx, d, it)
	case "host":
		return rowEdit[host.Host]{
			plural: "hosts", store: s.Host, validate: (*host.Host).Validate,
			metaOf: func(h *host.Host) *meta.Metadata { return &h.Meta },
			drop: func(h *host.Host) bool {
				listed := dropEntries(&h.Spec.Policies, is(t.id))
				isDefault := clearID(&h.Spec.DefaultPolicy, t.id)
				return listed || isDefault
			},
		}.apply(ctx, d, it)
	case "policy":
		return rowEdit[policy.Policy]{
			plural: "policies", store: s.Policy, validate: (*policy.Policy).Validate, guard: guardPolicyModels(d),
			metaOf: func(p *policy.Policy) *meta.Metadata { return &p.Meta },
			drop: func(p *policy.Policy) bool {
				models := dropEntries(&p.Spec.ModelIDs, is(t.id))
				hostKeys := dropEntries(&p.Spec.HostKeyIDs, is(t.id))
				limit := clearID(&p.Spec.RateLimitID, t.id)
				limits := dropEntries(&p.Spec.RLBindings, func(b policy.RLBinding) bool { return b.RateLimitID == t.id })
				return models || hostKeys || limit || limits
			},
		}.apply(ctx, d, it)
	case "pricing":
		return rowEdit[pricing.Pricing]{
			plural: "pricings", store: s.Pricing, validate: (*pricing.Pricing).Validate,
			metaOf: func(p *pricing.Pricing) *meta.Metadata { return &p.Meta },
			drop:   func(p *pricing.Pricing) bool { return dropKeepingOne(&p.Spec.TargetModelIDs, is(t.id)) },
		}.apply(ctx, d, it)
	case "host-binding":
		return rowEdit[binding.Binding]{
			plural: "host-bindings", store: s.Binding, validate: (*binding.Binding).Validate, guard: guardHostBinding(d),
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
		return rowEdit[policybinding.PolicyBinding]{
			plural: "policy-bindings", store: s.PolicyBinding, validate: (*policybinding.PolicyBinding).Validate, guard: guardPolicyBinding(d),
			metaOf: func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta },
			drop:   func(b *policybinding.PolicyBinding) bool { return dropKeepingOne(&b.Spec.Subjects, t.isSubject) },
		}.apply(ctx, d, it)
	case "user":
		return d.releaseRole(ctx, it, t)
	}
	return nil, nil
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
	if !editAllowed(d.Catalog, it.Kind, m.Owner.Kind, authz.IsAdmin(ctx)) {
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
	if _, err := updateUser(ctx, d.Users, d.Authz, nil, in); err != nil {
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

// isSubject reports whether a role or policy binding subject names t: a
// service account by id, a group by name.
func (t detachTarget) isSubject(s rolebinding.Subject) bool {
	want := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: t.id}
	if t.kind == "group" {
		want = rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: t.name}
	}
	return s.Key() == want.Key()
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
