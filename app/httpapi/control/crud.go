// CRUD handlers for the eight catalog kinds. Wired uniformly via
// registerKind[T]; per-kind glue (metaOf, slug resolver) lives in the
// registerCRUD block.
//
// Route surface per kind (no /control/ prefix — admin plane runs on its
// own listener):
//
//   GET    /{plural}                 list
//   GET    /{plural}/{ref}           read by slug or id (UUID form prefers id)
//   POST   /{plural}                 create  (server stamps id+slug)
//   PUT    /{plural}/by-id/{id}      update  (id-routed)
//   DELETE /{plural}/by-id/{id}      delete  (id-routed)

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
)

// entityStore is the slice of methods the CRUD factory needs from any
// app/X.Store. Each store satisfies this generic interface with T being
// its concrete entity type.
type entityStore[T any] interface {
	List(ctx context.Context) ([]*T, error)
	Get(ctx context.Context, id string) (*T, error)
	Upsert(ctx context.Context, t *T) error
	Delete(ctx context.Context, id string) error
}

// Generic input / output shapes for the CRUD ops. Declared at package
// level (not inside registerKind) so each generic instantiation is a
// distinct named type that huma's schema registry can resolve from
// $refs in the generated OpenAPI spec. An anonymous struct declared
// inside the generic function produces local types whose Name() is
// unstable, breaking $ref resolution for downstream codegen tools.
type listBody[T any] struct {
	Items []*T `json:"items"`
}
type listResponse[T any] struct {
	Body listBody[T]
}
type itemResponse[T any] struct {
	Body *T `json:"body"`
}
type createRequest[T any] struct {
	Body T `json:"body"`
}
type updateRequest[T any] struct {
	ID   string `path:"id" doc:"Resource id (UUIDv7)."`
	Body T      `json:"body"`
}

// Non-generic shared path-param inputs and the empty success body.
type refInput struct {
	Ref string `path:"ref" doc:"Resource slug or UUIDv7 id."`
}
type idInput struct {
	ID string `path:"id" doc:"Resource id (UUIDv7)."`
}
type emptyResponse struct{}

var errSlugNotFound = errors.New("not found")

// registerKind installs the five CRUD operations for kind T on api. The
// metaOf, validate, defaultOwnerKind, and resolveSlug closures supply
// the kind-specific glue.
//
// defaultOwnerKind is stamped on Create when the caller omits
// metadata.owner.kind. Pass "" for kinds where the caller must always
// supply owner.kind explicitly (e.g. Model needs Owner.Kind=provider
// with a specific Owner.ID; the API can't default it).
// mutationGuard runs before create/update/delete. action is "create",
// "update", or "delete". For create, existing is nil. For delete,
// incoming is nil. Return a non-nil error to block the mutation with 403.
type mutationGuard[T any] func(action string, existing, incoming *T) error

func registerKind[T any](
	api huma.API,
	plural, singular string,
	store entityStore[T],
	authzr authz.Authorizer,
	metaOf func(*T) *meta.Metadata,
	validate func(*T) error,
	defaultOwnerKind meta.OwnerKind,
	resolveSlug func(slug string) (string, error),
	guard mutationGuard[T],
	protect huma.Middlewares,
) {
	base := "/" + plural
	tag := plural

	// List
	huma.Register(api, huma.Operation{
		OperationID: "list_" + plural,
		Method:      http.MethodGet,
		Path:        base,
		Summary:     "List " + plural,
		Tags:        []string{tag},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*listResponse[T], error) {
		if err := authzr.Authorize(ctx, plural+".list", authz.Resource{Kind: singular}); err != nil {
			return nil, mapAuthzErr(err)
		}
		items, err := store.List(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if items == nil {
			items = []*T{}
		}
		out := &listResponse[T]{}
		out.Body.Items = items
		return out, nil
	})

	// Get by slug-or-id
	huma.Register(api, huma.Operation{
		OperationID: "get_" + singular,
		Method:      http.MethodGet,
		Path:        base + "/{ref}",
		Summary:     "Get " + singular + " by slug or id",
		Tags:        []string{tag},
		Middlewares: protect,
		Errors:      []int{401, 404, 500},
	}, func(ctx context.Context, in *refInput) (*itemResponse[T], error) {
		if err := authzr.Authorize(ctx, plural+".read", authz.Resource{Kind: singular, Name: in.Ref}); err != nil {
			return nil, mapAuthzErr(err)
		}
		id := in.Ref
		if !ids.Valid(id) {
			resolved, err := resolveSlug(id)
			if err != nil {
				return nil, huma.Error404NotFound(fmt.Sprintf("%s %q not found", singular, in.Ref))
			}
			id = resolved
		}
		v, err := store.Get(ctx, id)
		if err != nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s %q not found", singular, in.Ref))
		}
		return &itemResponse[T]{Body: v}, nil
	})

	// Create
	huma.Register(api, huma.Operation{
		OperationID:   "create_" + singular,
		Method:        http.MethodPost,
		Path:          base,
		Summary:       "Create " + singular,
		Tags:          []string{tag},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 500},
	}, func(ctx context.Context, in *createRequest[T]) (*itemResponse[T], error) {
		if err := authzr.Authorize(ctx, plural+".create", authz.Resource{Kind: singular}); err != nil {
			return nil, mapAuthzErr(err)
		}
		v := &in.Body
		m := metaOf(v)
		// Server stamps id+slug. Client-supplied id is discarded so id
		// provenance is auditable.
		m.ID = ids.New()
		if m.Name == "" {
			base := slug.From(m.DisplayName)
			if base == "" {
				base = singular
			}
			m.Name = slug.Unique(base, slugTakenFn(store, metaOf))
		}
		// API never creates system-owned rows. system is reserved for
		// seed paths (Store.Upsert directly, bypassing this handler).
		// When defaultOwnerKind is set, an empty owner gets stamped;
		// an explicit "system" gets rejected. Kinds without a default
		// (Model, HostKey) require the caller to specify owner.kind
		// because their valid owner is per-row.
		if m.Owner.Kind == meta.OwnerSystem {
			return nil, huma.Error400BadRequest("owner.kind=system is reserved for seed; omit owner.kind on create")
		}
		if m.Owner.Kind == "" && defaultOwnerKind != "" {
			m.Owner.Kind = defaultOwnerKind
		}
		// Validate AFTER stamping id+slug so the entity's Validate() sees
		// the same shape the store will persist. Rejecting here keeps bad
		// rows out of PG (which would otherwise break Bootstrap).
		if validate != nil {
			if err := validate(v); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
		}
		if guard != nil {
			if err := guard("create", nil, v); err != nil {
				return nil, huma.Error403Forbidden(err.Error())
			}
		}
		if err := store.Upsert(ctx, v); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		created, err := store.Get(ctx, m.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("created but could not read back: " + err.Error())
		}
		return &itemResponse[T]{Body: created}, nil
	})

	// Update by id
	huma.Register(api, huma.Operation{
		OperationID: "update_" + singular,
		Method:      http.MethodPut,
		Path:        base + "/by-id/{id}",
		Summary:     "Update " + singular + " by id",
		Tags:        []string{tag},
		Middlewares: protect,
		Errors:      []int{400, 401, 404, 500},
	}, func(ctx context.Context, in *updateRequest[T]) (*itemResponse[T], error) {
		if err := authzr.Authorize(ctx, plural+".update", authz.Resource{Kind: singular, ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		v := &in.Body
		m := metaOf(v)
		m.ID = in.ID // path id wins over body id
		if validate != nil {
			if err := validate(v); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
		}
		if guard != nil {
			if err := guard("update", existing, v); err != nil {
				return nil, huma.Error403Forbidden(err.Error())
			}
		}
		if err := store.Upsert(ctx, v); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		updated, err := store.Get(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("updated but could not read back: " + err.Error())
		}
		return &itemResponse[T]{Body: updated}, nil
	})

	// Delete by id
	huma.Register(api, huma.Operation{
		OperationID:   "delete_" + singular,
		Method:        http.MethodDelete,
		Path:          base + "/by-id/{id}",
		Summary:       "Delete " + singular + " by id",
		Tags:          []string{tag},
		Middlewares:   protect,
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{401, 404, 500},
	}, func(ctx context.Context, in *idInput) (*emptyResponse, error) {
		if err := authzr.Authorize(ctx, plural+".delete", authz.Resource{Kind: singular, ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		if guard != nil {
			if err := guard("delete", existing, nil); err != nil {
				return nil, huma.Error403Forbidden(err.Error())
			}
		}
		if err := store.Delete(ctx, in.ID); err != nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found: %s", singular, in.ID, err.Error()))
		}
		return &emptyResponse{}, nil
	})
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

// registerCRUD wires the eight kinds onto api. metaOf closures + slug
// resolvers are supplied per kind.
func registerCRUD(api huma.API, d Deps, protect huma.Middlewares) {
	pmeta := func(p *provider.Provider) *meta.Metadata { return &p.Meta }
	hmeta := func(h *host.Host) *meta.Metadata { return &h.Meta }
	mmeta := func(m *model.Model) *meta.Metadata { return &m.Meta }
	kmeta := func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta }
	rlmeta := func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }
	polmeta := func(p *policy.Policy) *meta.Metadata { return &p.Meta }
	prmeta := func(p *pricing.Pricing) *meta.Metadata { return &p.Meta }
	rkmeta := func(k *relaykey.RelayKey) *meta.Metadata { return &k.Meta }

	registerKind[provider.Provider](
		api, "providers", "provider", d.Stores.Provider, d.Authz, pmeta,
		func(p *provider.Provider) error { return p.Validate() },
		"",
		func(s string) (string, error) {
			p, ok := d.Catalog.Current().ProviderByName(s)
			if !ok {
				return "", errSlugNotFound
			}
			return p.Meta.ID, nil
		},
		nil,
		protect,
	)

	registerKind[host.Host](
		api, "hosts", "host", d.Stores.Host, d.Authz, hmeta,
		func(h *host.Host) error { return h.Validate() },
		"",
		func(s string) (string, error) {
			h, ok := d.Catalog.Current().HostByName(s)
			if !ok {
				return "", errSlugNotFound
			}
			return h.Meta.ID, nil
		},
		nil,
		protect,
	)

	registerKind[model.Model](
		api, "models", "model", d.Stores.Model, d.Authz, mmeta,
		func(m *model.Model) error { return m.Validate() },
		"",
		func(s string) (string, error) {
			ms := d.Catalog.Current().ModelsByName(s)
			if len(ms) == 0 {
				return "", errSlugNotFound
			}
			return ms[0].Meta.ID, nil
		},
		nil,
		protect,
	)

	registerKind[hostkey.HostKey](
		api, "host-keys", "host-key", d.Stores.HostKey, d.Authz, kmeta,
		func(k *hostkey.HostKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.HostKey, kmeta),
		nil,
		protect,
	)

	registerKind[ratelimit.RateLimit](
		api, "rate-limits", "rate-limit", d.Stores.RateLimit, d.Authz, rlmeta,
		func(r *ratelimit.RateLimit) error { return r.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RateLimit, rlmeta),
		rateLimitGuard,
		protect,
	)

	registerKind[policy.Policy](
		api, "policies", "policy", d.Stores.Policy, d.Authz, polmeta,
		func(p *policy.Policy) error { return p.Validate() },
		meta.OwnerUser,
		func(s string) (string, error) {
			p, ok := d.Catalog.Current().PolicyByName(s)
			if !ok {
				return "", errSlugNotFound
			}
			return p.Meta.ID, nil
		},
		policyGuard,
		protect,
	)

	registerKind[pricing.Pricing](
		api, "pricings", "pricing", d.Stores.Pricing, d.Authz, prmeta,
		func(p *pricing.Pricing) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Pricing, prmeta),
		nil,
		protect,
	)

	registerKind[relaykey.RelayKey](
		api, "relay-keys", "relay-key", d.Stores.RelayKey, d.Authz, rkmeta,
		func(k *relaykey.RelayKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RelayKey, rkmeta),
		nil,
		protect,
	)
}
