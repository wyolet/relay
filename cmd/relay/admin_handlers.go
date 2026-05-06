package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/db"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/configstore"
)

// adminKinds bundles the five kind handlers produced by the PER-265 factory.
type adminKinds struct {
	provider  *crud.Kind[*configstore.Provider]
	pool      *crud.Kind[*configstore.Pool]
	model     *crud.Kind[*configstore.Model]
	route     *crud.Kind[*configstore.Route]
	rateLimit *crud.Kind[*configstore.RateLimit]
}

func buildAdminKinds(store *configstore.PGStore, q *db.Queries) adminKinds {
	return adminKinds{
		provider:  providerKind(store, q),
		pool:      poolKind(store, q),
		model:     modelKind(store, q),
		route:     routeKind(store, q),
		rateLimit: rateLimitKind(store, q),
	}
}

// --- Provider ---

func providerKind(store *configstore.PGStore, q *db.Queries) *crud.Kind[*configstore.Provider] {
	return &crud.Kind[*configstore.Provider]{
		Name: "Provider",
		Decode: func(r *http.Request) (*configstore.Provider, error) {
			return decodeBody[configstore.Provider](r, func(v *configstore.Provider) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*configstore.Provider, error) {
			return store.Providers(), nil
		},
		Get: func(ctx context.Context, name string) (*configstore.Provider, error) {
			v, ok := store.ProviderByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, tx pgx.Tx, v *configstore.Provider) error {
			return upsertProvider(ctx, tx, v)
		},
		Update: func(ctx context.Context, tx pgx.Tx, name string, v *configstore.Provider) error {
			return upsertProvider(ctx, tx, v)
		},
		Delete: func(ctx context.Context, tx pgx.Tx, name string) error {
			return db.New(tx).DeleteProvider(ctx, name)
		},
		ResourceID: func(v *configstore.Provider) string { return v.Metadata.Name },
		Patch: func(v *configstore.Provider) configstore.Patch {
			return configstore.Patch{UpsertProvider: v}
		},
		PatchDelete: func(name string) configstore.Patch {
			return configstore.Patch{DeleteProvider: name}
		},
	}
}

func upsertProvider(ctx context.Context, tx pgx.Tx, v *configstore.Provider) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(v.Spec)
	if err != nil {
		return err
	}
	return db.New(tx).UpsertProvider(ctx, db.UpsertProviderParams{
		Name:     v.Metadata.Name,
		Metadata: meta,
		Spec:     spec,
	})
}

// --- Pool ---

func poolKind(store *configstore.PGStore, q *db.Queries) *crud.Kind[*configstore.Pool] {
	return &crud.Kind[*configstore.Pool]{
		Name: "Pool",
		Decode: func(r *http.Request) (*configstore.Pool, error) {
			return decodeBody[configstore.Pool](r, func(v *configstore.Pool) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*configstore.Pool, error) {
			return store.Pools(), nil
		},
		Get: func(ctx context.Context, name string) (*configstore.Pool, error) {
			v, ok := store.PoolByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, tx pgx.Tx, v *configstore.Pool) error {
			return upsertPool(ctx, tx, v)
		},
		Update: func(ctx context.Context, tx pgx.Tx, name string, v *configstore.Pool) error {
			return upsertPool(ctx, tx, v)
		},
		Delete: func(ctx context.Context, tx pgx.Tx, name string) error {
			return db.New(tx).DeletePool(ctx, name)
		},
		ResourceID: func(v *configstore.Pool) string { return v.Metadata.Name },
		Patch: func(v *configstore.Pool) configstore.Patch {
			return configstore.Patch{UpsertPool: v}
		},
		PatchDelete: func(name string) configstore.Patch {
			return configstore.Patch{DeletePool: name}
		},
	}
}

func upsertPool(ctx context.Context, tx pgx.Tx, v *configstore.Pool) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(v.Spec)
	if err != nil {
		return err
	}
	return db.New(tx).UpsertPool(ctx, db.UpsertPoolParams{
		Name:     v.Metadata.Name,
		Metadata: meta,
		Spec:     spec,
	})
}

// --- Model ---

func modelKind(store *configstore.PGStore, q *db.Queries) *crud.Kind[*configstore.Model] {
	return &crud.Kind[*configstore.Model]{
		Name: "Model",
		Decode: func(r *http.Request) (*configstore.Model, error) {
			return decodeBody[configstore.Model](r, func(v *configstore.Model) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*configstore.Model, error) {
			return store.Models(), nil
		},
		Get: func(ctx context.Context, name string) (*configstore.Model, error) {
			v, ok := store.ModelByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, tx pgx.Tx, v *configstore.Model) error {
			return upsertModel(ctx, tx, v)
		},
		Update: func(ctx context.Context, tx pgx.Tx, name string, v *configstore.Model) error {
			return upsertModel(ctx, tx, v)
		},
		Delete: func(ctx context.Context, tx pgx.Tx, name string) error {
			return db.New(tx).DeleteModel(ctx, name)
		},
		ResourceID: func(v *configstore.Model) string { return v.Metadata.Name },
		Patch: func(v *configstore.Model) configstore.Patch {
			return configstore.Patch{UpsertModel: v}
		},
		PatchDelete: func(name string) configstore.Patch {
			return configstore.Patch{DeleteModel: name}
		},
	}
}

func upsertModel(ctx context.Context, tx pgx.Tx, v *configstore.Model) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(v.Spec)
	if err != nil {
		return err
	}
	return db.New(tx).UpsertModel(ctx, db.UpsertModelParams{
		Name:     v.Metadata.Name,
		Metadata: meta,
		Spec:     spec,
	})
}

// --- Route ---

func routeKind(store *configstore.PGStore, q *db.Queries) *crud.Kind[*configstore.Route] {
	return &crud.Kind[*configstore.Route]{
		Name: "Route",
		Decode: func(r *http.Request) (*configstore.Route, error) {
			return decodeBody[configstore.Route](r, func(v *configstore.Route) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*configstore.Route, error) {
			return store.Routes(), nil
		},
		Get: func(ctx context.Context, name string) (*configstore.Route, error) {
			v, ok := store.RouteByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, tx pgx.Tx, v *configstore.Route) error {
			return upsertRoute(ctx, tx, v)
		},
		Update: func(ctx context.Context, tx pgx.Tx, name string, v *configstore.Route) error {
			return upsertRoute(ctx, tx, v)
		},
		Delete: func(ctx context.Context, tx pgx.Tx, name string) error {
			return db.New(tx).DeleteRoute(ctx, name)
		},
		ResourceID: func(v *configstore.Route) string { return v.Metadata.Name },
		Patch: func(v *configstore.Route) configstore.Patch {
			return configstore.Patch{UpsertRoute: v}
		},
		PatchDelete: func(name string) configstore.Patch {
			return configstore.Patch{DeleteRoute: name}
		},
	}
}

func upsertRoute(ctx context.Context, tx pgx.Tx, v *configstore.Route) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(v.Spec)
	if err != nil {
		return err
	}
	return db.New(tx).UpsertRoute(ctx, db.UpsertRouteParams{
		Name:     v.Metadata.Name,
		Metadata: meta,
		Spec:     spec,
	})
}

// --- RateLimit ---

func rateLimitKind(store *configstore.PGStore, q *db.Queries) *crud.Kind[*configstore.RateLimit] {
	return &crud.Kind[*configstore.RateLimit]{
		Name: "RateLimit",
		Decode: func(r *http.Request) (*configstore.RateLimit, error) {
			return decodeBody[configstore.RateLimit](r, func(v *configstore.RateLimit) error {
				if v.Metadata.Name == "" {
					return errors.New("metadata.name required")
				}
				return nil
			})
		},
		List: func(ctx context.Context) ([]*configstore.RateLimit, error) {
			return store.RateLimits(), nil
		},
		Get: func(ctx context.Context, name string) (*configstore.RateLimit, error) {
			v, ok := store.RateLimitByName(name)
			if !ok {
				return nil, crud.ErrNotFound
			}
			return v, nil
		},
		Insert: func(ctx context.Context, tx pgx.Tx, v *configstore.RateLimit) error {
			return upsertRateLimit(ctx, tx, v)
		},
		Update: func(ctx context.Context, tx pgx.Tx, name string, v *configstore.RateLimit) error {
			return upsertRateLimit(ctx, tx, v)
		},
		Delete: func(ctx context.Context, tx pgx.Tx, name string) error {
			return db.New(tx).DeleteRateLimit(ctx, name)
		},
		ResourceID: func(v *configstore.RateLimit) string { return v.Metadata.Name },
		Patch: func(v *configstore.RateLimit) configstore.Patch {
			return configstore.Patch{UpsertRateLimit: v}
		},
		PatchDelete: func(name string) configstore.Patch {
			return configstore.Patch{DeleteRateLimit: name}
		},
	}
}

func upsertRateLimit(ctx context.Context, tx pgx.Tx, v *configstore.RateLimit) error {
	meta, err := json.Marshal(v.Metadata)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(v.Spec)
	if err != nil {
		return err
	}
	return db.New(tx).UpsertRateLimit(ctx, db.UpsertRateLimitParams{
		Name:     v.Metadata.Name,
		Metadata: meta,
		Spec:     spec,
	})
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

// crudDeps constructs crud.Deps from the PGStore and its underlying pool.
func crudDeps(pool *pgxpool.Pool, store *configstore.PGStore) crud.Deps {
	return crud.DepsFromPGStore(pool, store, slog.Default())
}

// buildAdminCRUD calls Handlers() on each kind and bundles the results.
func buildAdminCRUD(kinds adminKinds, deps crud.Deps, store *configstore.PGStore) *adminCRUD {
	pl, pg, pc, pu, pd := kinds.provider.Handlers(deps)
	ol, og, oc, ou, od := kinds.pool.Handlers(deps)
	ml, mg, mc, mu, md := kinds.model.Handlers(deps)
	rl, rg, rc, ru, rd := kinds.route.Handlers(deps)
	ll, lg, lc, lu, ld := kinds.rateLimit.Handlers(deps)
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
	}
}

// mountAdminRoutes registers chi routes for all admin CRUD endpoints, gated by token check.
// Also mounts the unauthenticated POST /admin/login (cookie auth bootstrap).
func mountAdminRoutes(r chi.Router, tok string, h *adminCRUD, store *configstore.PGStore, deps crud.Deps) {
	gate := adminTokenGate(tok)

	// Login is NOT gated — it is the mechanism by which clients obtain a session cookie.
	// 401 is returned on wrong token (endpoint is publicly discoverable).
	r.Post("/admin/login", adminLoginHandler(tok))
	// Logout and whoami ARE gated — they require an existing valid session.
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
	// Attachments are managed inline on Pool/Secret/Model specs; no POST/DELETE here.
	r.With(gate).Get("/admin/attachments", h.attachmentList)

	// Misc admin endpoints (PER-275 version, PER-280 master-key generation).
	r.With(gate).Get("/admin/version", versionHandler())
	r.With(gate).Get("/admin/master-key/generate", masterKeyGenerateHandler())

	_ = store
	_ = deps
}

// adminTokenGate returns a chi middleware that checks the admin token.
// Accepted sources (in order): X-Relay-Admin-Token header, Authorization: Bearer header,
// relay_admin cookie (set by POST /admin/login).
// Returns 404 on mismatch — security-through-obscurity posture matching /admin/reload.
func adminTokenGate(token string) func(http.Handler) http.Handler {
	tok := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			adminTok := r.Header.Get("X-Relay-Admin-Token")
			if adminTok == "" {
				adminTok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
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
