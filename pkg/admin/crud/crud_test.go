package crud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
// in-memory store seeded with one system-owned and one user-owned RL.
func buildRateLimitKind() (*crud.Kind[*catalog.RateLimit], map[string]*catalog.RateLimit) {
	sysRule := catalog.RateLimitRule{
		Meter:    "requests",
		Amount:   100,
		Window:   time.Minute,
		Strategy: catalog.StrategySlidingWindow,
	}
	store := map[string]*catalog.RateLimit{
		"sys-rl": {
			Metadata: catalog.Metadata{
				Name:  "sys-rl",
				ID:    "00000000-0000-0000-0000-000000000001",
				Owner: catalog.Owner{Kind: catalog.OwnerSystem},
			},
			Spec: catalog.RateLimitSpec{Rules: []catalog.RateLimitRule{sysRule}},
		},
		"user-rl": {
			Metadata: catalog.Metadata{
				Name:  "user-rl",
				ID:    "00000000-0000-0000-0000-000000000002",
				Owner: catalog.Owner{Kind: catalog.OwnerUser},
			},
			Spec: catalog.RateLimitSpec{Rules: []catalog.RateLimitRule{sysRule}},
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
		Guard: func(_ context.Context, existing, proposed *catalog.RateLimit) error {
			if existing.Metadata.Owner.Kind == catalog.OwnerSystem {
				if proposed == nil {
					return huma.NewError(http.StatusForbidden,
						"system-owned RateLimit objects cannot be deleted")
				}
				// For testing: only allow amount edits.
				if len(proposed.Spec.Rules) != len(existing.Spec.Rules) {
					return huma.NewError(http.StatusForbidden,
						"system-owned RateLimit: rule count cannot be changed")
				}
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

func TestGuard_PUT_SystemOwned_Returns403(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	// Attempt to add a second rule — system guard should reject.
	body, _ := json.Marshal(&catalog.RateLimit{
		Metadata: catalog.Metadata{Name: "sys-rl", Owner: catalog.Owner{Kind: catalog.OwnerSystem}},
		Spec: catalog.RateLimitSpec{
			Rules: []catalog.RateLimitRule{
				{Meter: "requests", Amount: 100, Window: time.Minute, Strategy: catalog.StrategySlidingWindow},
				{Meter: "tokens", Amount: 1000, Window: time.Minute, Strategy: catalog.StrategySlidingWindow},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPut,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000001",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("PUT system-owned (rule count change): want 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestGuard_DELETE_SystemOwned_Returns403(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	req := httptest.NewRequest(http.MethodDelete,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000001",
		nil)
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("DELETE system-owned: want 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestGuard_DELETE_UserOwned_Succeeds(t *testing.T) {
	k, _ := buildRateLimitKind()
	api := buildTestAPI(t, k)

	req := httptest.NewRequest(http.MethodDelete,
		"/control/ratelimits/by-id/00000000-0000-0000-0000-000000000002",
		nil)
	w := httptest.NewRecorder()
	api.Adapter().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("DELETE user-owned: want 204, got %d (body: %s)", w.Code, w.Body.String())
	}
}
