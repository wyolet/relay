package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/auth/rbac"
)

// resource builds the authz.Resource the real request would authorize: a named row through its kind's own lookup, a collection with the owner its list or create route uses. Rows and owners the caller cannot see answer 404, as their routes do, so a review never reveals them.
func (r accessReviewer) resource(ctx context.Context, subject *actor.Actor, verb string, in reviewResourceInput) (authz.Resource, reviewedResource, error) {
	kr, known := r.rows[in.Kind]
	singular := authz.Singular(in.Kind)
	if known {
		singular = kr.singular
	}
	ref := in.ID
	if ref == "" {
		ref = in.Name
	}
	if ref != "" {
		if in.Owner != nil {
			return authz.Resource{}, reviewedResource{}, huma.Error400BadRequest("name a row or an owner, not both")
		}
		notFound := huma.Error404NotFound(fmt.Sprintf("%s %q not found", singular, ref))
		if !known {
			return authz.Resource{}, reviewedResource{}, notFound
		}
		res, err := kr.find(ctx, ref)
		if err != nil || !refcheck.Visible(ctx, r.authz, singular, res.ID, *res.Owner) {
			return authz.Resource{}, reviewedResource{}, notFound
		}
		return res, reviewedResource{Kind: singular, ID: res.ID, Name: res.Name, Owner: r.ownerRef(ctx, res.Owner)}, nil
	}

	res := authz.Resource{Kind: singular}
	switch {
	case in.Owner != nil:
		o, err := r.owner(ctx, *in.Owner)
		if err != nil {
			return authz.Resource{}, reviewedResource{}, err
		}
		res.Owner = &o
	case verb == rbac.VerbList && known && kr.listOwner != nil:
		o := *kr.listOwner
		res.Owner = &o
	case verb == "create" && known && kr.defaultOwner != "":
		res.Owner = &meta.Owner{Kind: kr.defaultOwner}
	}
	if verb == "create" && res.Owner != nil {
		// The create route fills a personal owner from whoever creates the row, here the subject.
		if err := refcheck.StampOwnerID(actor.WithActor(ctx, subject), res.Owner); err != nil {
			return authz.Resource{}, reviewedResource{}, huma.Error400BadRequest(err.Error())
		}
		if (res.Owner.Kind == meta.OwnerTeam || res.Owner.Kind == meta.OwnerProject) && res.Owner.ID == "" {
			return authz.Resource{}, reviewedResource{}, huma.Error400BadRequest(fmt.Sprintf("name the %s that would own the %s", res.Owner.Kind, singular))
		}
	}
	return res, reviewedResource{Kind: singular, Owner: r.ownerRef(ctx, res.Owner)}, nil
}

// owner resolves a named owner to the id its rows carry.
func (r accessReviewer) owner(ctx context.Context, in reviewOwnerInput) (meta.Owner, error) {
	kind := meta.OwnerKind(in.Kind)
	ref := in.ID
	if ref == "" {
		ref = in.Name
	}
	notFound := huma.Error404NotFound(fmt.Sprintf("owner %s %q not found", in.Kind, ref))
	switch kind {
	case meta.OwnerSystem:
		return meta.Owner{Kind: kind}, nil
	case meta.OwnerUser:
		caller := actor.From(ctx)
		switch {
		case in.ID != "" || in.Name == "":
			return meta.Owner{Kind: kind, ID: in.ID}, nil
		case caller != nil && caller.UserID != "" && in.Name == caller.Username:
			return meta.Owner{Kind: kind, ID: caller.UserID}, nil
		case r.users == nil || !authz.IsAdmin(ctx):
			// Looking up someone else's username would confirm the account exists.
			return meta.Owner{}, notFound
		}
		u, err := findUser(ctx, r.users, in.Name)
		if err != nil {
			return meta.Owner{}, notFound
		}
		return meta.Owner{Kind: kind, ID: u.ID}, nil
	case meta.OwnerTeam, meta.OwnerProject, meta.OwnerHost, meta.OwnerProvider:
		kr, ok := r.rows[in.Kind]
		if !ok || ref == "" {
			return meta.Owner{}, huma.Error400BadRequest(fmt.Sprintf("owner %s needs a name or an id", in.Kind))
		}
		res, err := kr.find(ctx, ref)
		if err != nil || !refcheck.Visible(ctx, r.authz, kr.singular, res.ID, *res.Owner) {
			return meta.Owner{}, notFound
		}
		return meta.Owner{Kind: kind, ID: res.ID}, nil
	}
	return meta.Owner{}, huma.Error400BadRequest(fmt.Sprintf("owner kind %q is not one of system, user, team, project, host, provider", in.Kind))
}
