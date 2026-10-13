package control

import (
	"context"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
)

// kindRows is how one kind's routes find a row and build the resource they authorize. The access review reads it so its answer is about the same resource the real request would authorize.
type kindRows struct {
	plural, singular string
	// defaultOwner is the owner kind create stamps when the body names none.
	defaultOwner meta.OwnerKind
	// listOwner is the owner the list route authorizes with; nil leaves the list unowned.
	listOwner *meta.Owner
	// find resolves a slug or an id to the resource the get route authorizes, named by the row's slug.
	find func(ctx context.Context, ref string) (authz.Resource, error)
}

// kindIndex holds every kind's rows under its plural and its singular.
type kindIndex map[string]kindRows

func (k kindIndex) add(r kindRows) {
	k[r.plural] = r
	k[r.singular] = r
}
