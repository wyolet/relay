package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/kv"
)

// adminKinds bundles the five kind handlers produced by the PER-265 factory.
type adminKinds struct {
	provider  *crud.Kind[*catalog.Provider]
	policy      *crud.Kind[*catalog.Policy]
	model     *crud.Kind[*catalog.Model]
	route     *crud.Kind[*catalog.Route]
	rateLimit *crud.Kind[*catalog.RateLimit]
}

func buildAdminKinds(store *catalog.PGStore, st *storage.Storage) adminKinds {
	return adminKinds{
		provider:  providerKind(store, st),
		policy:      policyKind(store, st),
		model:     modelKind(store, st),
		route:     routeKind(store, st),
		rateLimit: rateLimitKind(store, st),
	}
}

// --- Provider ---

func providerKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Provider] {
	return &crud.Kind[*catalog.Provider]{
		Name: "Provider",
		Decode: func(r *http.Request) (*catalog.Provider, error) {
			return decodeBody[catalog.Provider](r, func(v *catalog.Provider) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Provider, error) {
			return store.Providers(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.Provider, error) {
			v, ok := store.ProviderByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.Provider) error {
			return st.Catalog.UpsertProvider(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.Provider) error {
			return st.Catalog.UpsertProvider(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeleteProvider(ctx, name)
		},
		ResourceID: func(v *catalog.Provider) string { return v.Metadata.Name },
		Patch: func(v *catalog.Provider) catalog.Patch {
			return catalog.Patch{UpsertProvider: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeleteProvider: name}
		},
	}
}

// --- Policy ---

func policyKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Policy] {
	return &crud.Kind[*catalog.Policy]{
		Name: "Policy",
		Decode: func(r *http.Request) (*catalog.Policy, error) {
			return decodeBody[catalog.Policy](r, func(v *catalog.Policy) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Policy, error) {
			return store.Policies(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.Policy, error) {
			v, ok := store.PolicyByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.Policy) error {
			return st.Catalog.UpsertPolicy(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.Policy) error {
			return st.Catalog.UpsertPolicy(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeletePolicy(ctx, name)
		},
		ResourceID: func(v *catalog.Policy) string { return v.Metadata.Name },
		Patch: func(v *catalog.Policy) catalog.Patch {
			return catalog.Patch{UpsertPolicy: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeletePolicy: name}
		},
	}
}

// --- Model ---

func modelKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Model] {
	return &crud.Kind[*catalog.Model]{
		Name: "Model",
		Decode: func(r *http.Request) (*catalog.Model, error) {
			return decodeBody[catalog.Model](r, func(v *catalog.Model) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Model, error) {
			return store.Models(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.Model, error) {
			v, ok := store.ModelByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.Model) error {
			return st.Catalog.UpsertModel(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.Model) error {
			return st.Catalog.UpsertModel(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeleteModel(ctx, name)
		},
		ResourceID: func(v *catalog.Model) string { return v.Metadata.Name },
		Patch: func(v *catalog.Model) catalog.Patch {
			return catalog.Patch{UpsertModel: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeleteModel: name}
		},
	}
}

// --- Route ---

func routeKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Route] {
	return &crud.Kind[*catalog.Route]{
		Name: "Route",
		Decode: func(r *http.Request) (*catalog.Route, error) {
			return decodeBody[catalog.Route](r, func(v *catalog.Route) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Route, error) {
			return store.Routes(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.Route, error) {
			v, ok := store.RouteByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.Route) error {
			return st.Catalog.UpsertRoute(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.Route) error {
			return st.Catalog.UpsertRoute(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeleteRoute(ctx, name)
		},
		ResourceID: func(v *catalog.Route) string { return v.Metadata.Name },
		Patch: func(v *catalog.Route) catalog.Patch {
			return catalog.Patch{UpsertRoute: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeleteRoute: name}
		},
	}
}

// --- RateLimit ---

func rateLimitKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.RateLimit] {
	return &crud.Kind[*catalog.RateLimit]{
		Name: "RateLimit",
		Decode: func(r *http.Request) (*catalog.RateLimit, error) {
			return decodeBody[catalog.RateLimit](r, func(v *catalog.RateLimit) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.RateLimit, error) {
			return store.RateLimits(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.RateLimit, error) {
			v, ok := store.RateLimitByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.RateLimit) error {
			return st.Catalog.UpsertRateLimit(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.RateLimit) error {
			return st.Catalog.UpsertRateLimit(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeleteRateLimit(ctx, name)
		},
		ResourceID: func(v *catalog.RateLimit) string { return v.Metadata.Name },
		Patch: func(v *catalog.RateLimit) catalog.Patch {
			return catalog.Patch{UpsertRateLimit: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeleteRateLimit: name}
		},
	}
}

// --- helpers ---

// decodeBody reads r.Body, unmarshals into T, runs validate, returns pointer.
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

// storageTxRunner adapts *storage.Storage to the crud.TxRunner interface.
type storageTxRunner struct {
	st *storage.Storage
}

func (r *storageTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return r.st.WithTx(ctx, func(_ *storage.Storage) error {
		return fn(ctx)
	})
}

// crudDeps constructs crud.Deps from the PGStore and underlying storage.
func crudDeps(st *storage.Storage, store *catalog.PGStore) crud.Deps {
	return crud.Deps{
		Tx:       &storageTxRunner{st: st},
		Patcher:  store,
		Reloader: store,
		Logger:   slog.Default(),
	}
}

// adminTokenGate returns a chi middleware that checks the admin token.
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

// buildAdminCRUD constructs the adminCRUD bundle used by mountHuma.
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
