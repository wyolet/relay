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

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/filter"
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
	// Total is the match count before any limit/offset window — for "N of M"
	// displays. Equals len(Items) when the list isn't paginated.
	Total int `json:"total"`
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
type mutationGuard[T any] func(ctx context.Context, action string, existing, incoming *T) error

// enrichFn populates derived (non-stored) fields on a freshly-loaded entity
// before it's returned by list/get/create/update. Per the derived-field
// convention, target fields must carry `yaml:"-"` and a `// Derived:` doc
// comment at the field site (see app/hostkey/hostkey.go for the canonical
// example). Nil enrichFn is a no-op.
type enrichFn[T any] func(ctx context.Context, t *T)

// enrichListFn is the batch counterpart of enrichFn for list responses.
// When non-nil it replaces the per-item enrich loop on GET /{plural}, so
// derived fields that need a backend read (kv, another store) cost one
// batched read per request instead of one per row.
type enrichListFn[T any] func(ctx context.Context, items []*T)

// inUseFn refuses deleting a row anything still references (see
// Deps.refuseInUse). It runs last before store.Delete, once the caller is
// known to be allowed to delete the row.
type inUseFn func(ctx context.Context, kind, id, name string) error

// mergeOnUpdateFn copies fields the API allows to be omitted on update from
// the existing row onto the incoming body, before validate/upsert run.
// Used for write-only secrets where "no value shipped" means "keep
// existing" (e.g. hostkey stored-mode value); without this they'd fail
// Validate. Nil is a no-op.
type mergeOnUpdateFn[T any] func(existing, incoming *T)

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
	enrich enrichFn[T],
	enrichList enrichListFn[T],
	refuseInUse inUseFn,
	mergeUpdate mergeOnUpdateFn[T],
	gov settings.Reader,
	skipCreate bool,
	protect huma.Middlewares,
	filterSchema *filter.Schema[T],
) {
	base := "/" + plural
	tag := plural

	// List. When a filterSchema is supplied the route also parses the raw
	// query (stashed by withRawQuery) into a validated filter/sort/window;
	// unknown or malformed params become a 400 rather than silently matching
	// everything.
	listErrors := []int{401, 500}
	listMW := protect
	var listParams []*huma.Param
	if filterSchema != nil {
		listErrors = []int{400, 401, 500}
		listMW = withRawQuery(protect)
		listParams = filterParams(filterSchema)
	}
	huma.Register(api, huma.Operation{
		OperationID: "list_" + plural,
		Method:      http.MethodGet,
		Path:        base,
		Summary:     "List " + plural,
		Tags:        []string{tag},
		Middlewares: listMW,
		Parameters:  listParams,
		Errors:      listErrors,
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
		// Owner-scope BEFORE pagination so Total reflects the set the caller
		// may see, and before enrich so hidden rows aren't enriched.
		if s, ok := authzr.(authz.Scoper); ok {
			visible := items[:0:0]
			for _, it := range items {
				if s.Visible(ctx, singular, metaOf(it).ID, metaOf(it).Owner) {
					visible = append(visible, it)
				}
			}
			items = visible
		}
		if enrichList != nil {
			enrichList(ctx, items)
		} else if enrich != nil {
			for _, it := range items {
				enrich(ctx, it)
			}
		}
		out := &listResponse[T]{}
		if filterSchema != nil {
			q, err := filterSchema.Parse(rawQueryFrom(ctx))
			if err != nil {
				var fe *filter.Error
				if errors.As(err, &fe) {
					return nil, huma.Error400BadRequest(fe.Error())
				}
				return nil, huma.Error400BadRequest(err.Error())
			}
			page, total := q.Apply(items)
			if page == nil {
				page = []*T{}
			}
			out.Body.Items = page
			out.Body.Total = total
		} else {
			out.Body.Items = items
			out.Body.Total = len(items)
		}
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
		// Authorized on the fetched row: the decision needs its owner. 404,
		// not 403 — a row the caller may not see must not confirm its
		// existence.
		if err := authzr.Authorize(ctx, plural+".get",
			authz.Resource{Kind: singular, ID: id, Name: in.Ref, Owner: &metaOf(v).Owner}); err != nil {
			if errors.Is(err, authz.ErrUnauthenticated) {
				return nil, mapAuthzErr(err)
			}
			return nil, huma.Error404NotFound(fmt.Sprintf("%s %q not found", singular, in.Ref))
		}
		if enrich != nil {
			enrich(ctx, v)
		}
		return &itemResponse[T]{Body: v}, nil
	})

	// Create — skipped for kinds whose creation requires custom logic
	// (e.g. keys, which generate plaintext server-side and return
	// it once in the response body).
	if !skipCreate {
		huma.Register(api, huma.Operation{
			OperationID:   "create_" + singular,
			Method:        http.MethodPost,
			Path:          base,
			Summary:       "Create " + singular,
			Tags:          []string{tag},
			Middlewares:   protect,
			DefaultStatus: http.StatusCreated,
			Errors:        []int{400, 401, 403, 500},
		}, func(ctx context.Context, in *createRequest[T]) (*itemResponse[T], error) {
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
			// system is reserved for seed paths, except where it is the kind's
			// default (a personal Team, Group or Role would inherit whatever binds
			// to its name) and role-bindings, whose owner mirrors spec.scope.
			if m.Owner.Kind == meta.OwnerSystem && defaultOwnerKind != meta.OwnerSystem &&
				singular != "role-binding" {
				return nil, huma.Error400BadRequest("owner.kind=system is reserved for seed; omit owner.kind on create")
			}
			if m.Owner.Kind == "" && defaultOwnerKind != "" {
				m.Owner.Kind = defaultOwnerKind
			}
			// Roles stay authorable as personal rows (license-gated); a team or
			// group only ever names a shared scope.
			if (singular == "team" || singular == "group") && m.Owner.Kind != meta.OwnerSystem {
				return nil, huma.Error400BadRequest(singular + " owner.kind must be system; omit owner on create")
			}
			if err := stampOwnerID(ctx, &m.Owner); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
			// The guard runs first: on kinds whose owner mirrors a spec field
			// (Project, ServiceAccount, PolicyBinding) it is what re-derives
			// the owner, and the owner is what the decision below turns on.
			if guard != nil {
				if err := guard(ctx, "create", nil, v); err != nil {
					return nil, mapGuardErr(err)
				}
			}
			// Authorize AFTER owner stamping so an owner-aware Authorizer can
			// decide on the row's final provenance (user-owned rows are open to
			// any authenticated caller; anything else needs a binding).
			if err := authzr.Authorize(ctx, plural+".create", authz.Resource{Kind: singular, Owner: &m.Owner}); err != nil {
				return nil, mapAuthzErr(err)
			}
			// Validate AFTER stamping id+slug so the entity's Validate() sees
			// the same shape the store will persist. Rejecting here keeps bad
			// rows out of PG (which would otherwise break Bootstrap).
			if validate != nil {
				if err := validate(v); err != nil {
					return nil, huma.Error400BadRequest(err.Error())
				}
			}
			audit.Changed(ctx, []string{audit.AnyField})
			if err := store.Upsert(ctx, v); err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			created, err := store.Get(ctx, m.ID)
			if err != nil {
				return nil, huma.Error500InternalServerError("created but could not read back: " + err.Error())
			}
			if enrich != nil {
				enrich(ctx, created)
			}
			return &itemResponse[T]{Body: created}, nil
		})
	}

	// Update by id
	huma.Register(api, huma.Operation{
		OperationID: "update_" + singular,
		Method:      http.MethodPut,
		Path:        base + "/by-id/{id}",
		Summary:     "Update " + singular + " by id",
		Tags:        []string{tag},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *updateRequest[T]) (*itemResponse[T], error) {
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		if !visibleTo(ctx, authzr, singular, metaOf(existing).ID, metaOf(existing).Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		// Authorize with the fetched row's owner so an owner-aware Authorizer
		// can enforce owner.id == caller for user-owned rows.
		if err := authzr.Authorize(ctx, plural+".update", authz.Resource{Kind: singular, ID: in.ID, Owner: &metaOf(existing).Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if err := settings.Governs(gov, settings.OpEdit, singular, string(metaOf(existing).Owner.Kind), authz.IsAdmin(ctx)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		v := &in.Body
		m := metaOf(v)
		m.ID = in.ID // path id wins over body id
		if mergeUpdate != nil {
			mergeUpdate(existing, v)
		}
		// Owner is server-controlled provenance; PUT cannot chown a row.
		m.Owner = metaOf(existing).Owner
		if validate != nil {
			if err := validate(v); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
		}
		if guard != nil {
			if err := guard(ctx, "update", existing, v); err != nil {
				return nil, mapGuardErr(err)
			}
		}
		// Kinds whose owner mirrors a spec field re-derive it in validate or
		// the guard. A row moved that way lands in a new scope, which takes
		// the create grant there, as POST of the same body would.
		if m.Owner != metaOf(existing).Owner {
			if err := authzr.Authorize(ctx, plural+".create", authz.Resource{Kind: singular, Owner: &m.Owner}); err != nil {
				return nil, mapAuthzErr(err)
			}
		}
		audit.Changed(ctx, audit.DiffFields(existing, v))
		m.Dirty = true // operator-edited; seed must not clobber it on re-seed
		if err := store.Upsert(ctx, v); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		updated, err := store.Get(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("updated but could not read back: " + err.Error())
		}
		if enrich != nil {
			enrich(ctx, updated)
		}
		return &itemResponse[T]{Body: updated}, nil
	})

	// Delete by id. The route is always registered so the OpenAPI doc
	// advertises it for every kind (no `delete?: never` gaps in the
	// generated client); whether a delete actually succeeds is decided at
	// request time by the Authorizer, settings.Governs and the row's
	// references, not the spec shape.
	huma.Register(api, huma.Operation{
		OperationID:   "delete_" + singular,
		Method:        http.MethodDelete,
		Path:          base + "/by-id/{id}",
		Summary:       "Delete " + singular + " by id",
		Tags:          []string{tag},
		Middlewares:   protect,
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{401, 403, 404, 409, 500},
	}, func(ctx context.Context, in *idInput) (*emptyResponse, error) {
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		if !visibleTo(ctx, authzr, singular, metaOf(existing).ID, metaOf(existing).Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		if err := authzr.Authorize(ctx, plural+".delete", authz.Resource{Kind: singular, ID: in.ID, Owner: &metaOf(existing).Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if err := settings.Governs(gov, settings.OpDelete, singular, string(metaOf(existing).Owner.Kind), authz.IsAdmin(ctx)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		if guard != nil {
			if err := guard(ctx, "delete", existing, nil); err != nil {
				return nil, mapGuardErr(err)
			}
		}
		if refuseInUse != nil {
			if err := refuseInUse(ctx, singular, in.ID, metaOf(existing).Name); err != nil {
				return nil, err
			}
		}
		audit.Changed(ctx, []string{audit.AnyField})
		if err := store.Delete(ctx, in.ID); err != nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found: %s", singular, in.ID, err.Error()))
		}
		return &emptyResponse{}, nil
	})
}
