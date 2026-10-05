package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
)

func stampOwnerID(ctx context.Context, o *meta.Owner) error { return refcheck.StampOwnerID(ctx, o) }

func visibleTo(ctx context.Context, a authz.Authorizer, kind, id string, owner meta.Owner) bool {
	return refcheck.Visible(ctx, a, kind, id, owner)
}

// slugTakenFn returns the existence predicate slug.Unique needs to mint a
// non-colliding slug. Walks the store once per create — acceptable for
// catalogs in the hundreds; if it becomes a hotspot, the snapshot grows
// a byName index for the kinds that don't yet have one.
func slugTakenFn[T any](store entityStore[T], metaOf func(*T) *meta.Metadata) func(string) bool {
	taken := map[string]struct{}{}
	if items, err := store.List(context.Background()); err == nil {
		for _, it := range items {
			taken[metaOf(it).Name] = struct{}{}
		}
	}
	return func(candidate string) bool {
		_, ok := taken[candidate]
		return ok
	}
}

// mapGuardErr maps a mutationGuard error to an HTTP response. Guards that
// return a huma.StatusError (e.g. a 400 for an unresolvable ref) keep their
// chosen status; bare errors default to 403, matching the original
// "guard rejects = forbidden" contract.
func mapGuardErr(err error) error {
	var se huma.StatusError
	if errors.As(err, &se) {
		return err
	}
	return huma.Error403Forbidden(err.Error())
}

func mapAuthzErr(err error) error {
	switch {
	case errors.Is(err, authz.ErrUnauthenticated):
		return huma.Error401Unauthorized("unauthenticated")
	case errors.Is(err, authz.ErrForbidden):
		return huma.Error403Forbidden("forbidden")
	default:
		return huma.Error500InternalServerError("authz: " + err.Error())
	}
}

// staleVersionError is the 409 for an update whose metadata.resourceVersion
// no longer matches the stored row. The client refetches; it never retries
// the same body.
func staleVersionError(kind, id string) error {
	return &httpapi.APIError{
		Err: httpapi.APIErrorBody{
			Type:    "invalid_request_error",
			Code:    "stale_resource_version",
			Message: fmt.Sprintf("%s %q changed since you opened it; reload it and apply your change again", kind, id),
		},
		HTTPStatus: http.StatusConflict,
	}
}

// mapWriteErr maps a store write error: a stale resourceVersion is the
// caller's 409, anything else is a 500.
func mapWriteErr(kind, id string, err error) error {
	if errors.Is(err, meta.ErrStaleResourceVersion) {
		return staleVersionError(kind, id)
	}
	return huma.Error500InternalServerError(err.Error())
}

// listScanResolver is the slug→id resolver fallback for kinds whose
// snapshot doesn't have a byName index. Linear scan over store.List — OK
// for catalog sizes; revisit if the snapshot grows byName indices.
func listScanResolver[T any](store entityStore[T], metaOf func(*T) *meta.Metadata) func(string) (string, error) {
	return func(s string) (string, error) {
		items, err := store.List(context.Background())
		if err != nil {
			return "", err
		}
		for _, it := range items {
			if metaOf(it).Name == s {
				return metaOf(it).ID, nil
			}
		}
		return "", errSlugNotFound
	}
}
