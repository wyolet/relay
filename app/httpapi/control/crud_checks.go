package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
)

// kindChecks are the grant, governance and row checks a kind's CRUD routes run before a write. Routes that write rows of the kind on the way to something else call the same ones, so the two never drift.
type kindChecks[T any] struct {
	plural, singular string
	authz            authz.Authorizer
	gov              settings.Reader
	validate         func(*T) error
	guard            mutationGuard[T]
}

// mayCreate asks for the create grant in the scope owner names.
func (k kindChecks[T]) mayCreate(ctx context.Context, owner *meta.Owner) error {
	if err := k.authz.Authorize(ctx, k.plural+".create", authz.Resource{Kind: k.singular, Owner: owner}); err != nil {
		return mapAuthzErr(err)
	}
	return nil
}

// mayUpdate asks for the update grant on the stored row m and its governance for an edit.
func (k kindChecks[T]) mayUpdate(ctx context.Context, id string, m *meta.Metadata) error {
	if err := k.authz.Authorize(ctx, k.plural+".update", authz.Resource{Kind: k.singular, ID: id, Owner: &m.Owner}); err != nil {
		return mapAuthzErr(err)
	}
	if err := settings.Governs(k.gov, settings.OpEdit, k.singular, string(m.GovernedOwnerKind()), authz.IsAdmin(ctx)); err != nil {
		return huma.Error403Forbidden(err.Error())
	}
	return nil
}

// mayDelete asks for the delete grant on the stored row m and its governance for a delete.
func (k kindChecks[T]) mayDelete(ctx context.Context, id string, m *meta.Metadata) error {
	if err := k.authz.Authorize(ctx, k.plural+".delete", authz.Resource{Kind: k.singular, ID: id, Owner: &m.Owner}); err != nil {
		return mapAuthzErr(err)
	}
	if err := settings.Governs(k.gov, settings.OpDelete, k.singular, string(m.GovernedOwnerKind()), authz.IsAdmin(ctx)); err != nil {
		return huma.Error403Forbidden(err.Error())
	}
	return nil
}

// valid runs the kind's row rules, a 400 when the row breaks one.
func (k kindChecks[T]) valid(v *T) error {
	if k.validate == nil {
		return nil
	}
	if err := k.validate(v); err != nil {
		return huma.Error400BadRequest(err.Error())
	}
	return nil
}

// guarded runs the kind's cross-row guard for action.
func (k kindChecks[T]) guarded(ctx context.Context, action string, existing, incoming *T) error {
	if k.guard == nil {
		return nil
	}
	if err := k.guard(ctx, action, existing, incoming); err != nil {
		return mapGuardErr(err)
	}
	return nil
}
