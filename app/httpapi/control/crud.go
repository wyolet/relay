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
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
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
type mutationGuard[T any] func(action string, existing, incoming *T) error

// enrichFn populates derived (non-stored) fields on a freshly-loaded entity
// before it's returned by list/get/create/update. Per the derived-field
// convention, target fields must carry `yaml:"-"` and a `// Derived:` doc
// comment at the field site (see app/hostkey/hostkey.go for the canonical
// example). Nil enrichFn is a no-op.
type enrichFn[T any] func(ctx context.Context, t *T)

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
		if enrich != nil {
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
		if enrich != nil {
			enrich(ctx, v)
		}
		return &itemResponse[T]{Body: v}, nil
	})

	// Create — skipped for kinds whose creation requires custom logic
	// (e.g. relay-keys, which generate plaintext server-side and return
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
					return nil, mapGuardErr(err)
				}
			}
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
		Errors:      []int{400, 401, 404, 500},
	}, func(ctx context.Context, in *updateRequest[T]) (*itemResponse[T], error) {
		if err := authzr.Authorize(ctx, plural+".update", authz.Resource{Kind: singular, ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		// TODO(rbac): when multi-tenant RBAC lands, also enforce owner.id ==
		// caller for user-owned rows. Today the relay is single-user, so the
		// Authorizer above is permissive and Governs is the only guardrail.
		if err := settings.Governs(gov, settings.OpEdit, singular, string(metaOf(existing).Owner.Kind)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		v := &in.Body
		m := metaOf(v)
		m.ID = in.ID // path id wins over body id
		if mergeUpdate != nil {
			mergeUpdate(existing, v)
		}
		if validate != nil {
			if err := validate(v); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
		}
		if guard != nil {
			if err := guard("update", existing, v); err != nil {
				return nil, mapGuardErr(err)
			}
		}
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
		if err := authzr.Authorize(ctx, plural+".delete", authz.Resource{Kind: singular, ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		existing, err := store.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", singular, in.ID))
		}
		// TODO(rbac): when multi-tenant RBAC lands, also enforce owner.id ==
		// caller for user-owned rows. Today the relay is single-user, so the
		// Authorizer above is permissive and Governs is the only guardrail.
		if err := settings.Governs(gov, settings.OpDelete, singular, string(metaOf(existing).Owner.Kind)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		if guard != nil {
			if err := guard("delete", existing, nil); err != nil {
				return nil, huma.Error403Forbidden(err.Error())
			}
		}
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
	return func(action string, _, incoming *hostkey.HostKey) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		// Rotation must go through POST /host-keys/by-id/{id}/rotate,
		// not PUT — a stray value field on update would silently rotate
		// the credential.
		if action == "update" && incoming.Spec.ValueFrom.Kind == hostkey.ValueKindStored && incoming.Spec.Value != "" {
			return fmt.Errorf("value cannot be set on update — use POST /host-keys/by-id/{id}/rotate to rotate the credential")
		}
		// Cross-entity invariant: policy must be host-owned by the
		// hostkey's HostID. Per-row Validate() can't see other stores.
		if d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		pol, err := d.Stores.Policy.Get(context.Background(), incoming.Spec.PolicyID)
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

		if d.Stores.RelayKey != nil {
			rks, err := d.Stores.RelayKey.List(ctx)
			if err != nil {
				return fmt.Errorf("list relay-keys: %w", err)
			}
			for _, k := range rks {
				if k.Spec.PolicyID != id {
					continue
				}
				k.Spec.PolicyID = ""
				if err := d.Stores.RelayKey.Upsert(ctx, k); err != nil {
					return fmt.Errorf("detach from relay-key %q: %w", k.Meta.Name, err)
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

// mergeHostKeyPreserveValue treats an empty Spec.Value on a stored-mode
// update as "keep the existing key" — the caller wants to edit metadata
// or rebind to a different policy/host without rotating the credential.
// A non-empty Value still means rotation. Env-mode keys carry no value
// here, so this is a no-op for them.
func mergeHostKeyPreserveValue(existing, incoming *hostkey.HostKey) {
	if existing == nil || incoming == nil {
		return
	}
	if incoming.Spec.ValueFrom.Kind != hostkey.ValueKindStored {
		return
	}
	if incoming.Spec.Value != "" {
		return
	}
	incoming.Spec.Value = existing.Resolved
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
	rkmeta := func(k *relaykey.RelayKey) *meta.Metadata { return &k.Meta }

	registerKind[provider.Provider](
		api, "providers", "provider", d.Stores.Provider, d.Authz, pmeta,
		func(p *provider.Provider) error { return p.Validate() },
		"",
		listScanResolver(d.Stores.Provider, pmeta),
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
		d.Catalog,
		false,
		protect,
		&bindingFilter,
	)

	// relay-keys uses a custom POST handler (registerRelayKeyCreate) that
	// generates the bearer plaintext server-side and returns it once. The
	// generic CRUD POST is therefore skipped here.
	registerKind[relaykey.RelayKey](
		api, "relay-keys", "relay-key", d.Stores.RelayKey, d.Authz, rkmeta,
		func(k *relaykey.RelayKey) error { return k.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.RelayKey, rkmeta),
		nil,
		nil,
		nil,
		nil,
		d.Catalog,
		true, // skipCreate
		protect,
		&relayKeyFilter,
	)
	registerRelayKeyCreate(api, d, protect)
}
