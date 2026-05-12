package crud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/admin/crud"
)

// --- minimal fakes ---

type fakePatcher struct{}

func (fakePatcher) ValidateWithPatch(_ catalog.Patch) error { return nil }

type fakeTx struct{}

func (fakeTx) RunInTx(_ context.Context, fn func(context.Context) error) error {
	return fn(context.Background())
}

type fakeReloader struct{}

func (fakeReloader) Reload(_ context.Context) error { return nil }

// buildRateLimitKind builds a minimal crud.Kind[*catalog.RateLimit] with an
// in-memory store seeded with one system_mirrored and one user_defined RL.
func buildRateLimitKind() (*crud.Kind[*catalog.RateLimit], map[string]*catalog.RateLimit) {
	store := map[string]*catalog.RateLimit{
		"sys-rl": {
			Metadata: catalog.Metadata{Name: "sys-rl", ID: "00000000-0000-0000-0000-000000000001"},
			Spec:     catalog.RateLimitSpec{Source: string(catalog.SourceSystemMirrored)},
		},
		"user-rl": {
			Metadata: catalog.Metadata{Name: "user-rl", ID: "00000000-0000-0000-0000-000000000002"},
			Spec:     catalog.RateLimitSpec{Source: string(catalog.SourceUserDefined)},
		},
	}

	k := &crud.Kind[*catalog.RateLimit]{
		Name: "RateLimit",
		Decode: func(r *http.Request) (*catalog.RateLimit, error) {
			var v catalog.RateLimit
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				return nil, err
			}
			return &v, nil
		},
		List: func(_ context.Context) ([]*catalog.RateLimit, error) {
			var out []*catalog.RateLimit
			for _, v := range store {
				out = append(out, v)
			}
			return out, nil
		},
		GetBySlugOrID: func(_ context.Context, ref string) (*catalog.RateLimit, error) {
			if v, ok := store[ref]; ok {
				return v, nil
			}
			return nil, crud.ErrNotFound
		},
		GetByID: func(_ context.Context, id string) (*catalog.RateLimit, error) {
			for _, v := range store {
				if v.Metadata.ID == id {
					return v, nil
				}
			}
			return nil, crud.ErrNotFound
		},
		StampID: func(_ context.Context, _ *catalog.RateLimit) error { return nil },
		Insert: func(_ context.Context, v *catalog.RateLimit) error {
			store[v.Metadata.Name] = v
			return nil
		},
		UpdateByID: func(_ context.Context, id string, v *catalog.RateLimit) error {
			for _, existing := range store {
				if existing.Metadata.ID == id {
					store[existing.Metadata.Name] = v
					return nil
				}
			}
			return crud.ErrNotFound
		},
		DeleteByID: func(_ context.Context, id string) error {
			for name, v := range store {
				if v.Metadata.ID == id {
					delete(store, name)
					return nil
				}
			}
			return crud.ErrNotFound
		},
		ResourceID:      func(v *catalog.RateLimit) string { return v.Metadata.Name },
		ResourceIDValue: func(v *catalog.RateLimit) string { return v.Metadata.ID },
		Guard: func(_ context.Context, existing *catalog.RateLimit) error {
			if existing.Spec.Source == string(catalog.SourceSystemMirrored) {
				return huma.NewError(http.StatusForbidden,
					"system-mirrored RateLimit objects are read-only")
			}
			return nil
		},
	}
	return k, store
}

func buildTestAPI(t *testing.T, k *crud.Kind[*catalog.RateLimit]) huma.API {
	t.Helper()
	_, api := humatest.New(t, huma.DefaultConfig("test", "0.0.1"))
	deps := crud.Deps{
		Tx:       fakeTx{},
		Patcher:  fakePatcher{},
		Reloader: fakeReloader{},
		Logger:   slog.Default(),
	}
	crud.RegisterOps(api, "/control/ratelimits", "ratelimit", "ratelimits", k, deps, nil)
	return api
}

func TestGuard_PUT_SystemMirrored_Returns403(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	body, _ := json.Marshal(&catalog.RateLimit{
		Metadata: catalog.Metadata{Name: "sys-rl"},
		Spec:     catalog.RateLimitSpec{Source: string(catalog.SourceSystemMirrored)},
	})
	req := httptest.NewRequest(http.MethodPut,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000001",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("PUT system_mirrored: want 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestGuard_DELETE_SystemMirrored_Returns403(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	req := httptest.NewRequest(http.MethodDelete,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000001",
		nil)
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("DELETE system_mirrored: want 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestGuard_DELETE_UserDefined_Succeeds(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	req := httptest.NewRequest(http.MethodDelete,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000002",
		nil)
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE user_defined: want 204, got %d (body: %s)", w.Code, w.Body.String())
	}
}
