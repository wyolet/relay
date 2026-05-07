package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/admin/crud"
)

// adminKinds bundles the five kind handlers produced by the PER-265 factory.
type adminKinds struct {
	provider  *crud.Kind[*catalog.Provider]
	pool      *crud.Kind[*catalog.Pool]
	model     *crud.Kind[*catalog.Model]
	route     *crud.Kind[*catalog.Route]
	rateLimit *crud.Kind[*catalog.RateLimit]
}

func buildAdminKinds(store *catalog.PGStore, st *storage.Storage) adminKinds {
	return adminKinds{
		provider:  providerKind(store, st),
		pool:      poolKind(store, st),
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

// --- Pool ---

func poolKind(store *catalog.PGStore, st *storage.Storage) *crud.Kind[*catalog.Pool] {
	return &crud.Kind[*catalog.Pool]{
		Name: "Pool",
		Decode: func(r *http.Request) (*catalog.Pool, error) {
			return decodeBody[catalog.Pool](r, func(v *catalog.Pool) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*catalog.Pool, error) {
			return store.Pools(), nil
		},
		Get: func(ctx context.Context, name string) (*catalog.Pool, error) {
			v, ok := store.PoolByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, v *catalog.Pool) error {
			return st.Catalog.UpsertPool(ctx, *v)
		},
		Update: func(ctx context.Context, name string, v *catalog.Pool) error {
			return st.Catalog.UpsertPool(ctx, *v)
		},
		Delete: func(ctx context.Context, name string) error {
			return st.Catalog.DeletePool(ctx, name)
		},
		ResourceID: func(v *catalog.Pool) string { return v.Metadata.Name },
		Patch: func(v *catalog.Pool) catalog.Patch {
			return catalog.Patch{UpsertPool: v}
		},
		PatchDelete: func(name string) catalog.Patch {
			return catalog.Patch{DeletePool: name}
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

// buildAdminCRUD calls Handlers() on each kind and bundles the results.
func buildAdminCRUD(kinds adminKinds, deps crud.Deps, store *catalog.PGStore) *adminCRUD {
	pl, pg, pc, pu, pd := kinds.provider.Handlers(deps)
	ol, og, oc, ou, od := kinds.pool.Handlers(deps)
	ml, mg, mc, mu, md := kinds.model.Handlers(deps)
	rl, rg, rc, ru, rd := kinds.route.Handlers(deps)
	ll, lg, lc, lu, ld := kinds.rateLimit.Handlers(deps)
	depsCopy := deps
	kindsCopy := kinds
	return &adminCRUD{
		provider:  adminHandlers{pl, pg, pc, pu, pd},
		pool:      adminHandlers{ol, og, oc, ou, od},
		model:     adminHandlers{ml, mg, mc, mu, md},
		route:     adminHandlers{rl, rg, rc, ru, rd},
		rateLimit: adminHandlers{ll, lg, lc, lu, ld},

		secretList:     secretListHandler(store),
		secretGet:      secretGetHandler(store),
		secretCreate:   secretCreateHandler(store, deps),
		secretUpdate:   secretUpdateHandler(store, deps),
		secretDelete:   secretDeleteHandler(store, deps),
		attachmentList: attachmentListHandler(store, deps),

		version:           versionHandler(),
		masterKeyGenerate: masterKeyGenerateHandler(),

		// Typed fields for mountHuma full-schema registration.
		kinds:   &kindsCopy,
		deps:    &depsCopy,
		pgStore: store,
	}
}

// mountAdminRoutes registers chi routes for all admin CRUD endpoints, gated by token check.
// Also mounts the unauthenticated POST /admin/login (cookie auth bootstrap).
func mountAdminRoutes(r chi.Router, tok string, h *adminCRUD, store *catalog.PGStore, deps crud.Deps) {
	gate := adminTokenGate(tok)

	// Login is NOT gated — it is the mechanism by which clients obtain a session cookie.
	r.Post("/admin/login", adminLoginHandler(tok))
	r.With(gate).Post("/admin/logout", adminLogoutHandler())
	r.With(gate).Get("/admin/whoami", adminWhoamiHandler())

	type kindRoutes struct {
		plural   string
		handlers adminHandlers
	}
	kinds := []kindRoutes{
		{"providers", h.provider},
		{"pools", h.pool},
		{"models", h.model},
		{"routes", h.route},
		{"ratelimits", h.rateLimit},
	}
	for _, k := range kinds {
		k := k
		base := "/admin/" + k.plural
		r.With(gate).Get(base, k.handlers.list)
		r.With(gate).Post(base, k.handlers.create)
		r.With(gate).Get(base+"/{name}", k.handlers.get)
		r.With(gate).Put(base+"/{name}", k.handlers.update)
		r.With(gate).Delete(base+"/{name}", k.handlers.del)
	}

	// Secret endpoints (custom shapes, not via Kind[T] factory).
	r.With(gate).Get("/admin/secrets", h.secretList)
	r.With(gate).Post("/admin/secrets", h.secretCreate)
	r.With(gate).Get("/admin/secrets/{name}", h.secretGet)
	r.With(gate).Put("/admin/secrets/{name}", h.secretUpdate)
	r.With(gate).Delete("/admin/secrets/{name}", h.secretDelete)

	// Attachment endpoint — read-only derived view.
	r.With(gate).Get("/admin/attachments", h.attachmentList)

	// Misc admin endpoints.
	r.With(gate).Get("/admin/version", versionHandler())
	r.With(gate).Get("/admin/master-key/generate", masterKeyGenerateHandler())

	_ = store
	_ = deps
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
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
