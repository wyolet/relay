package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/slug"
)

// adminKinds bundles the kind handlers produced by the PER-265 factory.
type adminKinds struct {
	provider  *crud.Kind[*catalog.Provider]
	policy    *crud.Kind[*catalog.Policy]
	model     *crud.Kind[*catalog.Model]
	route     *crud.Kind[*catalog.Route]
	rateLimit *crud.Kind[*catalog.RateLimit]
	relayKey  *crud.Kind[*catalog.RelayKey]
}

func buildAdminKinds(store *catalog.PGStore, st *storage.Storage) adminKinds {
	return adminKinds{
		provider:  providerKind(store, st),
		policy:    policyKind(store, st),
		model:     modelKind(store, st),
		route:     routeKind(store, st),
		rateLimit: rateLimitKind(store, st),
		relayKey:  relayKeyKind(store, st),
	}
}

// stampMetaID is the shared StampID implementation. It assigns a fresh UUIDv7
// when meta.ID is empty and derives meta.Name from DisplayName when missing,
// using the supplied lookup to avoid slug collisions per kind.
func stampMetaID(meta *catalog.Metadata, slugTaken func(candidate string) bool) error {
	if meta.ID == "" {
		meta.ID = ids.New()
	}
	if meta.Name == "" {
		base := slug.From(meta.DisplayName)
		if base == "" {
			return errors.New("metadata.name or metadata.displayName required")
		}
		meta.Name = slug.Unique(base, slugTaken)
	}
	return nil
}

// --- Provider ---

func providerKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Provider] {
	return &crud.Kind[*catalog.Provider]{
		Name: "Provider",
		Decode: func(r *http.Request) (*catalog.Provider, error) {
			return decodeBody[catalog.Provider](r, func(v *catalog.Provider) error {
				if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
					return errors.New("metadata.name or metadata.displayName required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Provider, error) {
			return store.Providers(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.Provider, error) {
			if ids.Valid(ref) {
				if v, ok := store.ProviderByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.ProviderByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.Provider, error) {
			if v, ok := store.ProviderByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.Provider) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.ProviderByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.Provider) error {
			resolved, err := catalog.ResolvePolicyRef(store, v.Spec.DefaultPolicy)
			if err != nil {
				return err
			}
			v.Spec.DefaultPolicy = resolved
			return st.Catalog.UpsertProvider(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.Provider) error {
			v.Metadata.ID = id
			resolved, err := catalog.ResolvePolicyRef(store, v.Spec.DefaultPolicy)
			if err != nil {
				return err
			}
			v.Spec.DefaultPolicy = resolved
			return st.Catalog.UpsertProvider(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeleteProvider(ctx, id)
		},
		ResourceID:      func(v *catalog.Provider) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.Provider) string { return v.Metadata.ID },
		Owner:           func(v *catalog.Provider) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.Provider) catalog.Patch {
			return catalog.Patch{UpsertProvider: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeleteProvider: slug}
		},
	}
}

// --- Policy ---

func policyKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Policy] {
	return &crud.Kind[*catalog.Policy]{
		Name: "Policy",
		Decode: func(r *http.Request) (*catalog.Policy, error) {
			return decodeBody[catalog.Policy](r, func(v *catalog.Policy) error {
				if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
					return errors.New("metadata.name or metadata.displayName required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Policy, error) {
			return store.Policies(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.Policy, error) {
			if ids.Valid(ref) {
				if v, ok := store.PolicyByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.PolicyByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.Policy, error) {
			if v, ok := store.PolicyByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.Policy) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.PolicyByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.Policy) error {
			resolved, err := catalog.ResolveProviderRef(store, v.Spec.Provider)
			if err != nil {
				return err
			}
			v.Spec.Provider = resolved
			return st.Catalog.UpsertPolicy(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.Policy) error {
			v.Metadata.ID = id
			resolved, err := catalog.ResolveProviderRef(store, v.Spec.Provider)
			if err != nil {
				return err
			}
			v.Spec.Provider = resolved
			return st.Catalog.UpsertPolicy(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeletePolicy(ctx, id)
		},
		ResourceID:      func(v *catalog.Policy) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.Policy) string { return v.Metadata.ID },
		Owner:           func(v *catalog.Policy) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.Policy) catalog.Patch {
			return catalog.Patch{UpsertPolicy: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeletePolicy: slug}
		},
	}
}

// --- Model ---

func modelKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Model] {
	return &crud.Kind[*catalog.Model]{
		Name: "Model",
		Decode: func(r *http.Request) (*catalog.Model, error) {
			return decodeBody[catalog.Model](r, func(v *catalog.Model) error {
				if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
					return errors.New("metadata.name or metadata.displayName required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Model, error) {
			return store.Models(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.Model, error) {
			if ids.Valid(ref) {
				if v, ok := store.ModelByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.ModelByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.Model, error) {
			if v, ok := store.ModelByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.Model) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.ModelByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.Model) error {
			resolved, err := catalog.ResolveProviderRef(store, v.Spec.Provider)
			if err != nil {
				return err
			}
			v.Spec.Provider = resolved
			return st.Catalog.UpsertModel(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.Model) error {
			v.Metadata.ID = id
			resolved, err := catalog.ResolveProviderRef(store, v.Spec.Provider)
			if err != nil {
				return err
			}
			v.Spec.Provider = resolved
			return st.Catalog.UpsertModel(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeleteModel(ctx, id)
		},
		ResourceID:      func(v *catalog.Model) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.Model) string { return v.Metadata.ID },
		Owner:           func(v *catalog.Model) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.Model) catalog.Patch {
			return catalog.Patch{UpsertModel: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeleteModel: slug}
		},
	}
}

// --- Route ---

func routeKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Route] {
	return &crud.Kind[*catalog.Route]{
		Name: "Route",
		Decode: func(r *http.Request) (*catalog.Route, error) {
			return decodeBody[catalog.Route](r, func(v *catalog.Route) error {
				if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
					return errors.New("metadata.name or metadata.displayName required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Route, error) {
			return store.Routes(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.Route, error) {
			if ids.Valid(ref) {
				if v, ok := store.RouteByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.RouteByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.Route, error) {
			if v, ok := store.RouteByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.Route) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.RouteByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.Route) error {
			return st.Catalog.UpsertRoute(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.Route) error {
			v.Metadata.ID = id
			return st.Catalog.UpsertRoute(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeleteRoute(ctx, id)
		},
		ResourceID:      func(v *catalog.Route) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.Route) string { return v.Metadata.ID },
		Owner:           func(v *catalog.Route) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.Route) catalog.Patch {
			return catalog.Patch{UpsertRoute: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeleteRoute: slug}
		},
	}
}

// --- RateLimit ---

func rateLimitKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.RateLimit] {
	return &crud.Kind[*catalog.RateLimit]{
		Name: "RateLimit",
		Decode: func(r *http.Request) (*catalog.RateLimit, error) {
			return decodeBody[catalog.RateLimit](r, func(v *catalog.RateLimit) error {
				if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
					return errors.New("metadata.name or metadata.displayName required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.RateLimit, error) {
			return store.RateLimits(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.RateLimit, error) {
			if ids.Valid(ref) {
				if v, ok := store.RateLimitByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.RateLimitByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.RateLimit, error) {
			if v, ok := store.RateLimitByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.RateLimit) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.RateLimitByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.RateLimit) error {
			return st.Catalog.UpsertRateLimit(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.RateLimit) error {
			v.Metadata.ID = id
			return st.Catalog.UpsertRateLimit(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeleteRateLimit(ctx, id)
		},
		ResourceID:      func(v *catalog.RateLimit) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.RateLimit) string { return v.Metadata.ID },
		Owner:           func(v *catalog.RateLimit) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.RateLimit) catalog.Patch {
			return catalog.Patch{UpsertRateLimit: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeleteRateLimit: slug}
		},
		Guard: rateLimitGuard,
	}
}

// --- RelayKey ---

const relayKeyValueField = "value"

func relayKeyKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.RelayKey] {
	return &crud.Kind[*catalog.RelayKey]{
		Name: "RelayKey",
		Decode: func(r *http.Request) (*catalog.RelayKey, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			var v catalog.RelayKey
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			if v.Metadata.Name == "" && v.Metadata.DisplayName == "" {
				return nil, errors.New("metadata.name or metadata.displayName required")
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(raw, &envelope); err == nil {
				if rawVal, ok := envelope[relayKeyValueField]; ok {
					var val string
					if err := json.Unmarshal(rawVal, &val); err == nil && val != "" {
						sum := sha256.Sum256([]byte(val))
						v.Spec.KeyHash = hex.EncodeToString(sum[:])
						if v.Spec.Prefix == "" && len(val) >= 4 {
							v.Spec.Prefix = val[:min(8, len(val))]
						}
					}
				}
			}
			// On update without a fresh value, the handler will inherit the
			// existing hash from the by-id lookup before persistence.
			return &v, nil
		},
		List: func(ctx context.Context) ([]*catalog.RelayKey, error) {
			return store.RelayKeys(), nil
		},
		GetBySlugOrID: func(ctx context.Context, ref string) (*catalog.RelayKey, error) {
			if ids.Valid(ref) {
				if v, ok := store.RelayKeyByID(ref); ok {
					return v, nil
				}
			}
			if v, ok := store.RelayKeyByName(ref); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(ctx context.Context, id string) (*catalog.RelayKey, error) {
			if v, ok := store.RelayKeyByID(id); ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(ctx context.Context, v *catalog.RelayKey) error {
			return stampMetaID(&v.Metadata, func(s string) bool {
				_, ok := store.RelayKeyByName(s)
				return ok
			})
		},
		Insert: func(ctx context.Context, v *catalog.RelayKey) error {
			resolved, err := catalog.ResolvePolicyRef(store, v.Spec.PolicyRef)
			if err != nil {
				return err
			}
			v.Spec.PolicyRef = resolved
			return st.Catalog.UpsertRelayKey(ctx, *v)
		},
		UpdateByID: func(ctx context.Context, id string, v *catalog.RelayKey) error {
			v.Metadata.ID = id
			// Inherit existing hash/prefix when the update body omitted them.
			if v.Spec.KeyHash == "" {
				if existing, ok := store.RelayKeyByID(id); ok {
					v.Spec.KeyHash = existing.Spec.KeyHash
					if v.Spec.Prefix == "" {
						v.Spec.Prefix = existing.Spec.Prefix
					}
				}
			}
			resolved, err := catalog.ResolvePolicyRef(store, v.Spec.PolicyRef)
			if err != nil {
				return err
			}
			v.Spec.PolicyRef = resolved
			return st.Catalog.UpsertRelayKey(ctx, *v)
		},
		DeleteByID: func(ctx context.Context, id string) error {
			return st.Catalog.DeleteRelayKey(ctx, id)
		},
		ResourceID:      func(v *catalog.RelayKey) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.RelayKey) string { return v.Metadata.ID },
		Owner:           func(v *catalog.RelayKey) catalog.Owner { return v.Metadata.Owner },
		Patch: func(v *catalog.RelayKey) catalog.Patch {
			return catalog.Patch{UpsertRelayKey: v}
		},
		PatchDelete: func(slug string) catalog.Patch {
			return catalog.Patch{DeleteRelayKey: slug}
		},
	}
}

// --- RateLimit guard ---

// rateLimitGuard enforces per-owner edit rules:
//   - OwnerUser: unrestricted edit + delete.
//   - OwnerSystem / OwnerProvider: only spec.enabled and spec.rules[i].amount
//     (matched by index; length must not change) may differ; delete rejected.
func rateLimitGuard(_ context.Context, existing, proposed *catalog.RateLimit) error {
	switch existing.Metadata.Owner.Kind {
	case catalog.OwnerUser, "":
		// user-owned (or untagged legacy): no restrictions
		return nil
	case catalog.OwnerSystem, catalog.OwnerProvider:
		// delete is proposed==nil (zero value pointer)
		if proposed == nil {
			return huma.NewError(http.StatusForbidden,
				"system/provider-owned RateLimit objects cannot be deleted via the API")
		}
		return enforceSystemRateLimitAllowlist(existing, proposed)
	default:
		return huma.NewError(http.StatusForbidden,
			"unknown owner kind; edit rejected")
	}
}

// enforceSystemRateLimitAllowlist rejects any change to identity-style fields
// on system/provider-owned RateLimits. The rules array is operator-tunable
// (add, remove, edit any rule field) — Relay's runtime usage keys off the
// bucket NAME, not the rule shape, so rule edits do not break wiring.
//
// Locked: name, owner, description, displayName.
// Editable: enabled, rules[] (count and contents).
func enforceSystemRateLimitAllowlist(existing, proposed *catalog.RateLimit) error {
	if proposed.Metadata.Name != existing.Metadata.Name {
		return huma.NewError(http.StatusForbidden,
			"system/provider-owned RateLimit: metadata.name cannot be changed")
	}
	if proposed.Metadata.Owner != existing.Metadata.Owner {
		return huma.NewError(http.StatusForbidden,
			"system/provider-owned RateLimit: metadata.owner cannot be changed")
	}
	if proposed.Metadata.Description != existing.Metadata.Description {
		return huma.NewError(http.StatusForbidden,
			"system/provider-owned RateLimit: metadata.description cannot be changed")
	}
	if proposed.Metadata.DisplayName != existing.Metadata.DisplayName {
		return huma.NewError(http.StatusForbidden,
			"system/provider-owned RateLimit: metadata.displayName cannot be changed")
	}
	return nil
}

// --- helpers ---

func decodeBody[T any](r *http.Request, validate func(*T) error) (*T, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if err := validate(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

type storageTxRunner struct {
	st *storage.Storage
}

func (r *storageTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return r.st.WithTx(ctx, func(_ *storage.Storage) error {
		return fn(ctx)
	})
}

func crudDeps(st *storage.Storage, store *catalog.PGStore) crud.Deps {
	return crud.Deps{
		Tx:       &storageTxRunner{st: st},
		Patcher:  store,
		Reloader: store,
		Logger:   slog.Default(),
	}
}

func adminTokenGate(token string) func(http.Handler) http.Handler {
	tok := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			adminTok := r.Header.Get("X-Relay-Admin-Token")
			if adminTok == "" {
				if c, err := r.Cookie("relay_admin"); err == nil {
					adminTok = c.Value
				}
			}
			if subtle.ConstantTimeCompare([]byte(adminTok), tok) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"unauthenticated"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func buildAdminCRUD(kinds adminKinds, deps crud.Deps, store *catalog.PGStore, kvStore kv.Store) *adminCRUD {
	depsCopy := deps
	kindsCopy := kinds
	return &adminCRUD{
		kinds:   &kindsCopy,
		deps:    &depsCopy,
		pgStore: store,
		kvStore: kvStore,
	}
}

