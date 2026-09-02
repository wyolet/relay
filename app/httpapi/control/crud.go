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

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
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

// cascadeFn runs after the delete authz check and before store.Delete. It
// detaches the soon-to-be-deleted row from any referencing entities so the
// underlying FK constraints don't reject the delete. A non-nil error
// aborts the delete with a 500 — cascade failures shouldn't be silent,
// but the caller's request still fails closed.
type cascadeFn[T any] func(ctx context.Context, t *T) error

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
	cascade cascadeFn[T],
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
			// system is reserved for seed paths (Store.Upsert directly,
			// bypassing this handler) except on kinds whose default owner IS
			// system — Team, Group and Role name a scope or a grant, so a
			// personal row of those kinds would let any caller mint one and
			// inherit whatever already binds to its name. Kinds without a
			// default (Model, HostKey) require the caller to specify
			// owner.kind because their valid owner is per-row.
			if m.Owner.Kind == meta.OwnerSystem && defaultOwnerKind != meta.OwnerSystem {
				return nil, huma.Error400BadRequest("owner.kind=system is reserved for seed; omit owner.kind on create")
			}
			if m.Owner.Kind == "" && defaultOwnerKind != "" {
				m.Owner.Kind = defaultOwnerKind
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
		if err := settings.Governs(gov, settings.OpEdit, singular, string(metaOf(existing).Owner.Kind)); err != nil {
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
	// request time by settings.Governs + the Authorizer, not the spec shape.
	huma.Register(api, huma.Operation{
		OperationID:   "delete_" + singular,
		Method:        http.MethodDelete,
		Path:          base + "/by-id/{id}",
		Summary:       "Delete " + singular + " by id",
		Tags:          []string{tag},
		Middlewares:   protect,
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{401, 403, 404, 500},
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
		if err := settings.Governs(gov, settings.OpDelete, singular, string(metaOf(existing).Owner.Kind)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		if guard != nil {
			if err := guard(ctx, "delete", existing, nil); err != nil {
				return nil, huma.Error403Forbidden(err.Error())
			}
		}
		audit.Changed(ctx, []string{audit.AnyField})
		if cascade != nil {
			if err := cascade(ctx, existing); err != nil {
				return nil, huma.Error500InternalServerError("cascade: " + err.Error())
			}
		}
		if err := store.Delete(ctx, in.ID); err != nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found: %s", singular, in.ID, err.Error()))
		}
		return &emptyResponse{}, nil
	})
}

// stampOwnerID fills Owner.ID from the acting user on user-owned rows so
// ownership is recorded with an identity to key on. Admin-token callers
// carry no UserID: their rows keep an empty owner id and behave as
// operator/shared rows. A client-supplied owner.id must be truthful — only
// the break-glass admin token may set someone else's.
func stampOwnerID(ctx context.Context, o *meta.Owner) error {
	if o.Kind != meta.OwnerUser {
		return nil
	}
	a := actor.From(ctx)
	if a == nil {
		return nil
	}
	switch {
	case o.ID == "":
		o.ID = a.UserID
	case o.ID == a.UserID || a.AdminToken:
	default:
		return errors.New("owner.id must be empty or match the calling user")
	}
	return nil
}

// visibleTo reports whether the actor in ctx may see the identified row.
// True whenever the configured Authorizer doesn't scope reads (the
// single-user default).
func visibleTo(ctx context.Context, a authz.Authorizer, kind, id string, owner meta.Owner) bool {
	s, ok := a.(authz.Scoper)
	if !ok {
		return true
	}
	return s.Visible(ctx, kind, id, owner)
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

// guardHostKeyPolicyOwnership rejects hostkey create/update when the
// referenced Policy isn't host-owned by the key's HostID. Cross-entity
// invariant the per-row hostkey.Validate() can't enforce (it has no
// access to the policy store). Reads PG directly so disabled rows are
// considered too — a hostkey rebound to a disabled tier policy is still
// a structural mismatch, not just a soft drop. Delete is unaffected.
func guardHostKey(d Deps) mutationGuard[hostkey.HostKey] {
	return func(ctx context.Context, action string, existing, incoming *hostkey.HostKey) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		rotating := incoming.Spec.Value != "" && (existing == nil || incoming.Spec.Value != existing.Resolved)
		if action == "update" && rotating &&
			(incoming.Spec.ValueFrom.Kind == hostkey.ValueKindStored || incoming.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth) {
			return fmt.Errorf("value cannot be set on update — use POST /host-keys/by-id/{id}/rotate to rotate the credential")
		}
		// Cross-entity invariant: policy must be host-owned by the
		// hostkey's HostID. Per-row Validate() can't see other stores.
		if d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		pol, err := d.Stores.Policy.Get(ctx, incoming.Spec.PolicyID)
		if err != nil || pol == nil {
			return fmt.Errorf("policy %q does not exist", incoming.Spec.PolicyID)
		}
		if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != incoming.Spec.HostID {
			return fmt.Errorf("policy %q is not host-owned by host %q (owner=%s/%s)",
				pol.Meta.Name, incoming.Spec.HostID, pol.Meta.Owner.Kind, pol.Meta.Owner.ID)
		}
		return nil
	}
}

// guardKeyPolicy rejects a key mutation whose Spec.PolicyID
// points at a policy the caller may not see — a key inherits its
// policy's host-keys, so binding to a foreign policy would route traffic
// through someone else's credentials. Reported as "not found" to avoid
// confirming the row exists. Existence of the policy is otherwise still
// not checked here (the inference path handles missing policies).
func guardKeyPolicy(d Deps) mutationGuard[key.Key] {
	return func(ctx context.Context, action string, _, incoming *key.Key) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return checkPolicyRefVisible(ctx, d, incoming.Spec.PolicyID, incoming.Meta.Owner)
	}
}

// guardServiceAccount re-derives the account's owner from spec.projectId
// on every write (spec is the source of truth, owner mirrors it) and
// rejects a project or policy the caller may not see.
func guardServiceAccount(d Deps) mutationGuard[serviceaccount.ServiceAccount] {
	return func(ctx context.Context, action string, _, incoming *serviceaccount.ServiceAccount) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		incoming.StampOwner()
		if err := checkProjectRefVisible(ctx, d, incoming.Spec.ProjectID); err != nil {
			return err
		}
		return checkPolicyRefVisible(ctx, d, incoming.Spec.PolicyID, incoming.Meta.Owner)
	}
}

// checkProjectRefVisible rejects a row whose project doesn't exist (400) or
// isn't visible to the caller (404 — a project the caller may not see must
// not be confirmed to exist).
func checkProjectRefVisible(ctx context.Context, d Deps, projectID string) error {
	if d.Stores == nil || d.Stores.Project == nil {
		return nil
	}
	p, err := d.Stores.Project.Get(ctx, projectID)
	if err != nil || p == nil {
		return huma.Error400BadRequest(fmt.Sprintf("project %q does not exist", projectID))
	}
	s, ok := d.Authz.(authz.Scoper)
	if !ok {
		return nil
	}
	if !s.Visible(ctx, "project", p.Meta.ID, p.Meta.Owner) {
		return huma.Error404NotFound(fmt.Sprintf("project %q not found", projectID))
	}
	return nil
}

// guardGroupMembers rejects a member id that is not a user. Membership is
// what a role binding resolves through, so a typo'd id would silently
// grant nothing.
func guardGroupMembers(d Deps) mutationGuard[group.Group] {
	return func(ctx context.Context, action string, _, incoming *group.Group) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if d.Users == nil {
			return nil
		}
		for _, id := range incoming.Spec.MemberIDs {
			u, err := d.Users.Get(ctx, id)
			if err != nil || u == nil {
				return huma.Error400BadRequest(fmt.Sprintf("user %q does not exist", id))
			}
		}
		return nil
	}
}

// guardProject re-derives the project's owner from spec.teamId on every
// write (spec is the source of truth, owner mirrors it) and rejects a team
// the caller may not see.
func guardProject(d Deps) mutationGuard[project.Project] {
	return func(ctx context.Context, action string, _, incoming *project.Project) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		incoming.StampOwner()
		return checkTeamRefVisible(ctx, d, incoming.Spec.TeamID)
	}
}

// checkTeamRefVisible rejects a project whose team doesn't exist (400) or
// isn't visible to the caller (404 — a team the caller may not see must
// not be confirmed to exist).
func checkTeamRefVisible(ctx context.Context, d Deps, teamID string) error {
	if d.Stores == nil || d.Stores.Team == nil {
		return nil
	}
	t, err := d.Stores.Team.Get(ctx, teamID)
	if err != nil || t == nil {
		return huma.Error400BadRequest(fmt.Sprintf("team %q does not exist", teamID))
	}
	s, ok := d.Authz.(authz.Scoper)
	if !ok {
		return nil
	}
	if !s.Visible(ctx, "team", t.Meta.ID, t.Meta.Owner) {
		return huma.Error404NotFound(fmt.Sprintf("team %q not found", teamID))
	}
	return nil
}

// checkPolicyRefVisible rejects a policy the caller may not see, and a
// project-owned policy referenced from a personal (user-owned) row: a
// project's upstream credentials are reached through a ServiceAccount in
// that project, so the traffic carries its attribution and limits (D51).
func checkPolicyRefVisible(ctx context.Context, d Deps, policyID string, refOwner meta.Owner) error {
	if policyID == "" {
		return nil
	}
	s, ok := d.Authz.(authz.Scoper)
	if !ok || d.Stores == nil || d.Stores.Policy == nil {
		return nil
	}
	p, err := d.Stores.Policy.Get(ctx, policyID)
	if err != nil || p == nil {
		return nil
	}
	if !s.Visible(ctx, "policy", p.Meta.ID, p.Meta.Owner) {
		return huma.Error400BadRequest(fmt.Sprintf("policy %q not found", policyID))
	}
	if refOwner.Kind == meta.OwnerUser && p.Meta.Owner.Kind == meta.OwnerProject {
		return huma.Error400BadRequest("personal rows cannot reference project resources")
	}
	return nil
}

// checkHostKeyRefsVisible rejects host-key ids the caller may not see — a
// policy referencing a foreign host-key would spend someone else's upstream
// credential. Missing rows pass through (host-key existence is deliberately
// not checked at policy write time; the inference path handles it).
func checkHostKeyRefsVisible(ctx context.Context, d Deps, keyIDs []string, refOwner meta.Owner) error {
	if len(keyIDs) == 0 {
		return nil
	}
	s, ok := d.Authz.(authz.Scoper)
	if !ok || d.Stores == nil || d.Stores.HostKey == nil {
		return nil
	}
	for _, id := range keyIDs {
		k, err := d.Stores.HostKey.Get(ctx, id)
		if err != nil || k == nil {
			continue
		}
		if !s.Visible(ctx, "host-key", k.Meta.ID, k.Meta.Owner) {
			return huma.Error400BadRequest(fmt.Sprintf("host-key %q not found", id))
		}
		// Same rule as checkPolicyRefVisible: a personal policy must not
		// spend a project's upstream credential (D51).
		if refOwner.Kind == meta.OwnerUser && k.Meta.Owner.Kind == meta.OwnerProject {
			return huma.Error400BadRequest("personal rows cannot reference project resources")
		}
	}
	return nil
}

// enrichHostStatus returns an enrichFn that overlays observed runtime health
// (host.Status) onto a freshly-loaded Host from the host-health store. The
// field is derived (json:"status", yaml:"-") and never persisted; nil when no
// observation exists yet (no traffic / TTL'd out) so the UI shows "unknown".
func enrichHostStatus(d Deps) enrichFn[host.Host] {
	return func(ctx context.Context, h *host.Host) {
		if h == nil || d.HostHealth == nil {
			return
		}
		if st, found := d.HostHealth.Read(ctx, h.Meta.ID); found {
			s := st
			h.Status = &s
		}
	}
}

// enrichHostStatusAll is the list-path variant: one kv Range for every host's
// health record instead of a Get per row.
func enrichHostStatusAll(d Deps) enrichListFn[host.Host] {
	return func(ctx context.Context, hosts []*host.Host) {
		if len(hosts) == 0 || d.HostHealth == nil {
			return
		}
		statuses := d.HostHealth.ReadAll(ctx)
		if len(statuses) == 0 {
			return
		}
		for _, h := range hosts {
			if st, found := statuses[h.Meta.ID]; found {
				s := st
				h.Status = &s
			}
		}
	}
}

// enrichHostKeyPolicies returns an enrichFn that fills HostKey.Policies
// with the user Policies that reference this key via Spec.HostKeyIDs,
// read off the current catalog snapshot. Reverse-ref summary for the
// admin UI; never persisted (the field is yaml:"-" and skipped by the
// store).
func enrichHostKeyPolicies(d Deps) enrichFn[hostkey.HostKey] {
	return func(ctx context.Context, k *hostkey.HostKey) {
		if k == nil || d.Stores == nil || d.Stores.Policy == nil {
			return
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return
		}
		var refs []hostkey.PolicyRef
		for _, p := range pols {
			for _, id := range p.Spec.HostKeyIDs {
				if id == k.Meta.ID {
					refs = append(refs, hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
					break
				}
			}
		}
		k.Policies = refs
	}
}

// enrichHostKeyPoliciesAll is the list-path variant: one Policy.List for the
// whole page instead of one per key row (the former N+1 on /api/host-keys).
func enrichHostKeyPoliciesAll(d Deps) enrichListFn[hostkey.HostKey] {
	return func(ctx context.Context, keys []*hostkey.HostKey) {
		if len(keys) == 0 || d.Stores == nil || d.Stores.Policy == nil {
			return
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return
		}
		byKey := map[string][]hostkey.PolicyRef{}
		for _, p := range pols {
			seen := map[string]bool{}
			for _, id := range p.Spec.HostKeyIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				byKey[id] = append(byKey[id], hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
			}
		}
		for _, k := range keys {
			k.Policies = byKey[k.Meta.ID]
		}
	}
}

// cascadeHostKeyDetach returns a cascade that strips the deleted HostKey's
// id from every Policy.Spec.HostKeyIDs that references it. Required because
// the policy_host_keys join table FK-constrains a HostKey delete; without
// detachment Postgres rejects with SQLSTATE 23503. Walks the Policy store
// directly (not the snapshot) so disabled policies are caught too.
func cascadeHostKeyDetach(d Deps) cascadeFn[hostkey.HostKey] {
	return func(ctx context.Context, k *hostkey.HostKey) error {
		if k == nil || d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return fmt.Errorf("list policies: %w", err)
		}
		for _, p := range pols {
			before := p.Spec.HostKeyIDs
			filtered := before[:0:0]
			changed := false
			for _, id := range before {
				if id == k.Meta.ID {
					changed = true
					continue
				}
				filtered = append(filtered, id)
			}
			if !changed {
				continue
			}
			p.Spec.HostKeyIDs = filtered
			if err := d.Stores.Policy.Upsert(ctx, p); err != nil {
				return fmt.Errorf("detach from policy %q: %w", p.Meta.Name, err)
			}
		}
		return nil
	}
}

// cascadePolicyDetach scrubs every JSONB reference to the deleted Policy
// before the row is removed:
//   - relay_keys.spec.policyId → cleared; the key becomes policy-less and
//     follows settings.Inference.AllowMissingPolicy on the hot path.
//   - host_keys.spec.policyId → cleared; the key is left without a tier
//     policy and is dropped from the snapshot by sanitizeHostKey until
//     reattached.
//   - hosts.spec.policies[] entries equal to this id → removed.
//   - hosts.spec.defaultPolicy equal to this id → cleared.
//
// PG-side FKs only cover the join tables (policy_models, policy_host_keys
// — both CASCADE). Everything else lives in spec JSONB and needs app-
// level cleanup.
func cascadePolicyDetach(d Deps) cascadeFn[policy.Policy] {
	return func(ctx context.Context, p *policy.Policy) error {
		if p == nil || d.Stores == nil {
			return nil
		}
		id := p.Meta.ID

		if d.Stores.Key != nil {
			rks, err := d.Stores.Key.List(ctx)
			if err != nil {
				return fmt.Errorf("list keys: %w", err)
			}
			for _, k := range rks {
				if k.Spec.PolicyID != id {
					continue
				}
				k.Spec.PolicyID = ""
				if err := d.Stores.Key.Upsert(ctx, k); err != nil {
					return fmt.Errorf("detach from key %q: %w", k.Meta.Name, err)
				}
			}
		}

		if d.Stores.HostKey != nil {
			keys, err := d.Stores.HostKey.List(ctx)
			if err != nil {
				return fmt.Errorf("list host-keys: %w", err)
			}
			for _, k := range keys {
				if k.Spec.PolicyID != id {
					continue
				}
				k.Spec.PolicyID = ""
				if err := d.Stores.HostKey.Upsert(ctx, k); err != nil {
					return fmt.Errorf("detach from host-key %q: %w", k.Meta.Name, err)
				}
			}
		}

		if d.Stores.Host != nil {
			hosts, err := d.Stores.Host.List(ctx)
			if err != nil {
				return fmt.Errorf("list hosts: %w", err)
			}
			for _, h := range hosts {
				changed := false
				if h.Spec.DefaultPolicy == id {
					h.Spec.DefaultPolicy = ""
					changed = true
				}
				if len(h.Spec.Policies) > 0 {
					filtered := make([]string, 0, len(h.Spec.Policies))
					for _, pid := range h.Spec.Policies {
						if pid == id {
							changed = true
							continue
						}
						filtered = append(filtered, pid)
					}
					if changed {
						if len(filtered) == 0 {
							h.Spec.Policies = nil
						} else {
							h.Spec.Policies = filtered
						}
					}
				}
				if !changed {
					continue
				}
				if err := d.Stores.Host.Upsert(ctx, h); err != nil {
					return fmt.Errorf("detach from host %q: %w", h.Meta.Name, err)
				}
			}
		}
		return nil
	}
}

// cascadeRateLimitDetach strips the deleted RateLimit id from every
// policy's Spec.RLBindings before the row is removed. The flat
// policies.rate_limit_id column is already handled by PG (FK SET NULL),
// but RLBindings lives in the spec JSONB and PG can't touch it.
// Without this, a deleted RL would leave dangling binding ids that the
// catalog snapshot would silently drop on reload — workable, but the
// data plane sees a stale view until reload runs.
func cascadeRateLimitDetach(d Deps) cascadeFn[ratelimit.RateLimit] {
	return func(ctx context.Context, r *ratelimit.RateLimit) error {
		if r == nil || d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return fmt.Errorf("list policies: %w", err)
		}
		for _, p := range pols {
			if len(p.Spec.RLBindings) == 0 {
				continue
			}
			filtered := make([]policy.RLBinding, 0, len(p.Spec.RLBindings))
			changed := false
			for _, b := range p.Spec.RLBindings {
				if b.RateLimitID == r.Meta.ID {
					changed = true
					continue
				}
				filtered = append(filtered, b)
			}
			if !changed {
				continue
			}
			if len(filtered) == 0 {
				p.Spec.RLBindings = nil
			} else {
				p.Spec.RLBindings = filtered
			}
			if err := d.Stores.Policy.Upsert(ctx, p); err != nil {
				return fmt.Errorf("detach from policy %q: %w", p.Meta.Name, err)
			}
		}
		return nil
	}
}

// mergeHostKeyPreserveValue treats an empty Spec.Value on a stored- or
// oauth-mode update as "keep the existing credential" — the caller wants to
// edit metadata or rebind to a different policy/host without rotating it. A
// non-empty Value still means rotation. Env-mode keys carry no value here, so
// this is a no-op for them.
func mergeHostKeyPreserveValue(existing, incoming *hostkey.HostKey) {
	if existing == nil || incoming == nil {
		return
	}
	if incoming.Spec.Value != "" {
		return // explicit new value → rotation
	}
	switch incoming.Spec.ValueFrom.Kind {
	case hostkey.ValueKindStored:
		// Re-supply the existing secret so the store re-encrypts it unchanged.
		incoming.Spec.Value = existing.Resolved
	case hostkey.ValueKindOAuth:
		// existing.Resolved is the access token, NOT the stored token blob, so
		// it can't be re-encrypted as the value. Carry Resolved so Validate
		// passes; the store preserves the existing blob ciphertext as-is.
		incoming.Resolved = existing.Resolved
	}
}

// guardRole reserves the built-in role names for the seeded system rows and
// gates authoring a custom role on the license. Delete stays open so an
// expired license never traps a row an operator wants gone.
func guardRole(d Deps) mutationGuard[role.Role] {
	return func(_ context.Context, action string, existing, incoming *role.Role) error {
		// Built-ins are the relay's own rows: every binding in a fresh
		// deployment points at one, so neither edit nor delete goes through
		// generic CRUD.
		if existing != nil && role.IsBuiltin(existing.Meta.Name) {
			return huma.Error403Forbidden(fmt.Sprintf("role %q is built in: %s goes through the seed, not generic CRUD", existing.Meta.Name, action))
		}
		if action == "delete" || incoming == nil {
			return nil
		}
		if role.IsBuiltin(incoming.Meta.Name) {
			return huma.Error400BadRequest(fmt.Sprintf("role name %q is reserved for the built-in role", incoming.Meta.Name))
		}
		if d.License == nil || !d.License.Has(license.FeatureCustomRoles) {
			return huma.Error403Forbidden(license.ErrRequired.Error())
		}
		return nil
	}
}

// guardRoleBinding re-derives the binding's owner from spec.scope (spec is
// the source of truth, owner mirrors it) and rejects a role, a scope target,
// or an id-bearing subject the caller may not see. Group subjects are
// unchecked: an IdP group has no row.
func guardRoleBinding(d Deps) mutationGuard[rolebinding.RoleBinding] {
	return func(ctx context.Context, action string, _, incoming *rolebinding.RoleBinding) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		incoming.StampOwner()
		if err := checkRoleRefVisible(ctx, d, incoming.Spec.RoleID); err != nil {
			return err
		}
		switch incoming.Spec.Scope.Kind {
		case meta.OwnerTeam:
			if err := checkTeamRefVisible(ctx, d, incoming.Spec.Scope.ID); err != nil {
				return err
			}
		case meta.OwnerProject:
			if err := checkProjectRefVisible(ctx, d, incoming.Spec.Scope.ID); err != nil {
				return err
			}
		}
		return checkSubjectsExist(ctx, d, incoming.Spec.Subjects)
	}
}

// guardPolicyBinding re-derives the binding's owner from spec.projectId,
// rejects a project, policy, or subject the caller may not see, and applies
// the default priority so the stored row orders against explicit values.
func guardPolicyBinding(d Deps) mutationGuard[policybinding.PolicyBinding] {
	return func(ctx context.Context, action string, _, incoming *policybinding.PolicyBinding) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		incoming.StampOwner()
		if incoming.Spec.Priority == 0 {
			incoming.Spec.Priority = policybinding.DefaultPriority
		}
		if err := checkProjectRefVisible(ctx, d, incoming.Spec.ProjectID); err != nil {
			return err
		}
		if err := checkPolicyRefExists(ctx, d, incoming.Spec.PolicyID, incoming.Meta.Owner); err != nil {
			return err
		}
		return checkSubjectsExist(ctx, d, incoming.Spec.Subjects)
	}
}

// checkRoleRefVisible rejects a binding whose role does not exist (400) or
// is not visible to the caller (404).
func checkRoleRefVisible(ctx context.Context, d Deps, roleID string) error {
	if d.Stores == nil || d.Stores.Role == nil {
		return nil
	}
	r, err := d.Stores.Role.Get(ctx, roleID)
	if err != nil || r == nil {
		return huma.Error400BadRequest(fmt.Sprintf("role %q does not exist", roleID))
	}
	s, ok := d.Authz.(authz.Scoper)
	if !ok {
		return nil
	}
	if !s.Visible(ctx, "role", r.Meta.ID, r.Meta.Owner) {
		return huma.Error404NotFound(fmt.Sprintf("role %q not found", roleID))
	}
	return nil
}

// checkPolicyRefExists rejects a binding whose policy does not exist, then
// applies the same visibility rule the other policy refs use.
func checkPolicyRefExists(ctx context.Context, d Deps, policyID string, refOwner meta.Owner) error {
	if d.Stores == nil || d.Stores.Policy == nil {
		return nil
	}
	p, err := d.Stores.Policy.Get(ctx, policyID)
	if err != nil || p == nil {
		return huma.Error400BadRequest(fmt.Sprintf("policy %q does not exist", policyID))
	}
	return checkPolicyRefVisible(ctx, d, policyID, refOwner)
}

// checkSubjectsExist rejects an id-bearing subject that names no row. Group
// subjects carry a name an IdP may supply, so they are never checked.
func checkSubjectsExist(ctx context.Context, d Deps, subjects []rolebinding.Subject) error {
	for i := range subjects {
		sub := &subjects[i]
		switch sub.Kind {
		case rolebinding.SubjectUser:
			if d.Users == nil {
				continue
			}
			u, err := d.Users.Get(ctx, sub.ID)
			if err != nil || u == nil {
				return huma.Error400BadRequest(fmt.Sprintf("user %q does not exist", sub.ID))
			}
		case rolebinding.SubjectServiceAccount:
			if d.Stores == nil || d.Stores.ServiceAccount == nil {
				continue
			}
			sa, err := d.Stores.ServiceAccount.Get(ctx, sub.ID)
			if err != nil || sa == nil {
				return huma.Error400BadRequest(fmt.Sprintf("service account %q does not exist", sub.ID))
			}
		}
	}
	return nil
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
	bmeta := func(b *binding.Binding) *meta.Metadata { return &b.Meta }
	rkmeta := func(k *key.Key) *meta.Metadata { return &k.Meta }
	tmeta := func(t *team.Team) *meta.Metadata { return &t.Meta }
	projmeta := func(p *project.Project) *meta.Metadata { return &p.Meta }
	sameta := func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta }
	gmeta := func(g *group.Group) *meta.Metadata { return &g.Meta }
	rolemeta := func(r *role.Role) *meta.Metadata { return &r.Meta }
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	pbmeta := func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta }

	registerKind[team.Team](
		api, "teams", "team", d.Stores.Team, d.Authz, tmeta,
		func(t *team.Team) error { return t.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Team, tmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&teamFilter,
	)

	registerKind[project.Project](
		api, "projects", "project", d.Stores.Project, d.Authz, projmeta,
		func(p *project.Project) error { return p.Validate() },
		meta.OwnerTeam,
		listScanResolver(d.Stores.Project, projmeta),
		guardProject(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&projectFilter,
	)

	registerKind[serviceaccount.ServiceAccount](
		api, "service-accounts", "service-account", d.Stores.ServiceAccount, d.Authz, sameta,
		func(sa *serviceaccount.ServiceAccount) error { return sa.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.ServiceAccount, sameta),
		guardServiceAccount(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&serviceAccountFilter,
	)

	registerKind[group.Group](
		api, "groups", "group", d.Stores.Group, d.Authz, gmeta,
		func(g *group.Group) error { return g.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Group, gmeta),
		guardGroupMembers(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&groupFilter,
	)

	registerKind[role.Role](
		api, "roles", "role", d.Stores.Role, d.Authz, rolemeta,
		func(r *role.Role) error { return r.Validate() },
		meta.OwnerSystem,
		listScanResolver(d.Stores.Role, rolemeta),
		guardRole(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&roleFilter,
	)

	registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", d.Stores.RoleBinding, d.Authz, rbmeta,
		// Owner mirrors the scope exactly, so it is re-derived before the
		// row is checked rather than in the guard that runs after.
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver(d.Stores.RoleBinding, rbmeta),
		guardRoleBinding(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&roleBindingFilter,
	)

	registerKind[policybinding.PolicyBinding](
		api, "policy-bindings", "policy-binding", d.Stores.PolicyBinding, d.Authz, pbmeta,
		func(b *policybinding.PolicyBinding) error { return b.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.PolicyBinding, pbmeta),
		guardPolicyBinding(d),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&policyBindingFilter,
	)

	registerKind[provider.Provider](
		api, "providers", "provider", d.Stores.Provider, d.Authz, pmeta,
		func(p *provider.Provider) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Provider, pmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&providerFilter,
	)

	registerKind[host.Host](
		api, "hosts", "host", d.Stores.Host, d.Authz, hmeta,
		func(h *host.Host) error { return h.Validate() },
		"",
		listScanResolver(d.Stores.Host, hmeta),
		nil,
		enrichHostStatus(d),
		enrichHostStatusAll(d),
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&hostFilter,
	)

	registerKind[model.Model](
		api, "models", "model", d.Stores.Model, d.Authz, mmeta,
		func(m *model.Model) error { return m.Validate() },
		"",
		listScanResolver(d.Stores.Model, mmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&modelFilter,
	)

	registerKind[hostkey.HostKey](
		api, "host-keys", "host-key", d.Stores.HostKey, d.Authz, kmeta,
		func(k *hostkey.HostKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.HostKey, kmeta),
		guardHostKey(d),
		enrichHostKeyPolicies(d),
		enrichHostKeyPoliciesAll(d),
		cascadeHostKeyDetach(d),
		mergeHostKeyPreserveValue,
		d.Catalog,
		false,
		protect,
		&hostKeyFilter,
	)

	registerKind[ratelimit.RateLimit](
		api, "rate-limits", "rate-limit", d.Stores.RateLimit, d.Authz, rlmeta,
		func(r *ratelimit.RateLimit) error { return r.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RateLimit, rlmeta),
		nil,
		nil,
		nil,
		cascadeRateLimitDetach(d),
		nil,
		d.Catalog,
		false,
		protect,
		&rateLimitFilter,
	)

	registerKind[policy.Policy](
		api, "policies", "policy", d.Stores.Policy, d.Authz, polmeta,
		func(p *policy.Policy) error { return p.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Policy, polmeta),
		guardPolicyModels(d),
		nil,
		nil,
		cascadePolicyDetach(d),
		nil,
		d.Catalog,
		false,
		protect,
		&policyFilter,
	)

	registerKind[pricing.Pricing](
		api, "pricings", "pricing", d.Stores.Pricing, d.Authz, prmeta,
		func(p *pricing.Pricing) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Pricing, prmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&pricingFilter,
	)

	registerKind[binding.Binding](
		api, "host-bindings", "host-binding", d.Stores.Binding, d.Authz, bmeta,
		func(b *binding.Binding) error { return b.Validate() },
		"",
		listScanResolver(d.Stores.Binding, bmeta),
		nil,
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		false,
		protect,
		&bindingFilter,
	)

	// keys uses a custom POST handler (registerKeyCreate) that
	// generates the bearer plaintext server-side and returns it once. The
	// generic CRUD POST is therefore skipped here.
	registerKind[key.Key](
		api, "keys", "key", d.Stores.Key, d.Authz, rkmeta,
		func(k *key.Key) error { return k.Validate() },
		meta.OwnerProject,
		listScanResolver(d.Stores.Key, rkmeta),
		guardKeyPolicy(d),
		nil,
		nil,
		nil,
		// Credential material is server-managed: PUT can neither wipe nor
		// overwrite it. Rotation goes through POST /keys/by-id/{id}/rotate.
		func(existing, incoming *key.Key) {
			incoming.Spec.KeyHash = existing.Spec.KeyHash
			incoming.Spec.Prefix = existing.Spec.Prefix
			incoming.Spec.PreviousKeyHash = existing.Spec.PreviousKeyHash
			incoming.Spec.GraceUntil = existing.Spec.GraceUntil
			incoming.Spec.Principal = existing.Spec.Principal
		},
		d.Catalog,
		true, // skipCreate
		protect,
		&keyFilter,
	)
	registerKeyCreate(api, d, protect)
}
